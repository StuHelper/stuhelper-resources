package resources_test

import (
	"context"
	"errors"
	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"net/http/httptest"
	"strings"
	"testing"
)

type countingInspectFiles struct {
	*files
	inspects int
}

func (f *countingInspectFiles) Inspect(ctx context.Context, p r.Principal, id string) (r.FileEvidence, error) {
	f.inspects++
	return f.files.Inspect(ctx, p, id)
}

type hookDiscoverAuthority struct {
	*authority
	onDiscover func()
}

func (a *hookDiscoverAuthority) Check(ctx context.Context, p r.Principal, action, scope string) error {
	err := a.authority.Check(ctx, p, action, scope)
	if action == "discover" && a.onDiscover != nil {
		cb := a.onDiscover
		a.onDiscover = nil
		cb()
	}
	return err
}

func TestIdempotencyGuardsConcurrentRequestPinningAndConfigValidation(t *testing.T) {
	h := newFixture(t)
	cif := &countingInspectFiles{files: h.files}
	hda := &hookDiscoverAuthority{authority: h.auth}
	var e error
	h.s, e = r.New(h.repo, r.Dependencies{Authority: hda, Catalog: h.catalog, Files: cif, Moderation: h.moderation}, h.cfg)
	must(t, e)

	draft := h.draft(t, "ig-draft", "public")
	published := h.publish(t, h.draft(t, "ig-pub", "public"))

	// A-11.1: Invalid Idempotency-Key must be rejected BEFORE calling Authority, Catalog or Files.
	catBefore := h.catalog.calls
	inspBefore := cif.inspects
	h.auth.mu.Lock()
	h.auth.calls = map[string]int{}
	h.auth.mu.Unlock()

	for _, badKey := range []string{"", "bad key!", strings.Repeat("k", 129)} {
		if _, err := h.s.CreateDraft(h.ctx, uploader, badKey, r.DraftWrite{Metadata: metadata("public")}); !errors.Is(err, r.ErrInput) {
			t.Fatalf("CreateDraft(%q) expected ErrInput, got %v", badKey, err)
		}
		if _, err := h.s.UpdateDraft(h.ctx, uploader, draft.ID, badKey, r.DraftWrite{Revision: draft.Revision, Metadata: metadata("public")}); !errors.Is(err, r.ErrInput) {
			t.Fatalf("UpdateDraft(%q) expected ErrInput, got %v", badKey, err)
		}
		if _, err := h.s.AttachFile(h.ctx, uploader, draft.ID, badKey, r.FileLink{Revision: draft.Revision, MediaVersionID: "file-ig-draft"}); !errors.Is(err, r.ErrInput) {
			t.Fatalf("AttachFile(%q) expected ErrInput, got %v", badKey, err)
		}
		if _, err := h.s.PutDirectory(h.ctx, manager, "dir-1", badKey, r.DirectoryWrite{SchoolID: "school", Names: r.Names{"zh-CN": "目录"}, Enabled: true}); !errors.Is(err, r.ErrInput) {
			t.Fatalf("PutDirectory(%q) expected ErrInput, got %v", badKey, err)
		}
	}
	h.auth.mu.Lock()
	authCalls := len(h.auth.calls)
	h.auth.mu.Unlock()
	if h.catalog.calls != catBefore || cif.inspects != inspBefore || authCalls != 0 {
		t.Fatalf("invalid Idempotency-Key leaked to dependencies: catalogDelta=%d inspectDelta=%d authCalls=%d", h.catalog.calls-catBefore, cif.inspects-inspBefore, authCalls)
	}

	// A-11.2: Reusing an already-used Idempotency-Key with different PackageRequest (even nonexistent IDs) must return 409 ErrConflict, not 404.
	pkg, e := h.s.CreatePackage(h.ctx, reader, "pkg-idem-key", r.PackageRequest{ResourceIDs: []string{published.ID}})
	must(t, e)
	if pkg.ID == "" {
		t.Fatal("empty package ID")
	}
	if _, err := h.s.CreatePackage(h.ctx, reader, "pkg-idem-key", r.PackageRequest{ResourceIDs: []string{"nonexistent-id"}}); !errors.Is(err, r.ErrConflict) {
		t.Fatalf("CreatePackage payload mismatch with same key expected ErrConflict, got %v", err)
	}

	// Conflicting replay on CreateDraft must also fail with ErrConflict without calling Catalog.Validate again.
	_, e = h.s.CreateDraft(h.ctx, uploader, "draft-idem-key", r.DraftWrite{Metadata: metadata("public")})
	must(t, e)
	catAfterFirst := h.catalog.calls
	changedMeta := metadata("public")
	changedMeta.Title = "篡改标题"
	if _, err := h.s.CreateDraft(h.ctx, uploader, "draft-idem-key", r.DraftWrite{Metadata: changedMeta}); !errors.Is(err, r.ErrConflict) {
		t.Fatalf("CreateDraft payload mismatch expected ErrConflict, got %v", err)
	}
	if h.catalog.calls != catAfterFirst {
		t.Fatalf("conflicting replay unnecessarily called Catalog.Validate (%d -> %d)", catAfterFirst, h.catalog.calls)
	}

	// A-11.3: CreateRequest must lock & re-read target Record inside transaction so concurrent version activation is captured.
	hda.onDiscover = func() {
		updatedMeta := metadata("public")
		updatedMeta.Title = "并发激活的新版本标题"
		upd, err := h.s.UpdateDraft(h.ctx, uploader, published.ID, "concurrent-upd", r.DraftWrite{Revision: published.Revision, Metadata: updatedMeta})
		must(t, err)
		published = h.publish(t, upd)
	}
	receipt, e := h.s.CreateRequest(h.ctx, reader, published.ID, "concurrent-complaint", r.Request{
		Kind:     "complaint",
		Category: "copyright",
		Reason:   "版权核实",
		Contact:  "reader@example.invalid",
		Evidence: []string{"https://example.invalid/evidence"},
	})
	must(t, e)
	if receipt.ResourceTitle != "并发激活的新版本标题" {
		t.Fatalf("CreateRequest used stale pre-tx record title: got %q", receipt.ResourceTitle)
	}
	var pinnedVersion string
	must(t, h.admin.QueryRow(`SELECT data->>'VersionID' FROM resources.requests WHERE id=$1`, receipt.ID).Scan(&pinnedVersion))
	if pinnedVersion != published.Current.ID {
		t.Fatalf("CreateRequest pinned stale VersionID %s instead of current %s", pinnedVersion, published.Current.ID)
	}

	// A-16.1: RetryRequest with invalid path ID must return 400 ErrInput (not 404).
	if _, err := h.s.RetryRequest(h.ctx, uploader, "invalid!id", "retry-k"); !errors.Is(err, r.ErrInput) {
		t.Fatalf("RetryRequest invalid ID expected ErrInput, got %v", err)
	}
	handler, e := httpapi.New(h.s, authenticator{})
	must(t, e)
	req := httptest.NewRequest("POST", "/api/v1/resources/requests/invalid!id/retry", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer owner")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "http-retry-k")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("HTTP RetryRequest invalid ID expected 400, got %d %s", w.Code, w.Body.String())
	}

	// A-16.3: service.New validates UploadProfiles entries.
	for _, badProfiles := range [][]r.UploadProfile{
		{{SchoolID: "school", Access: "invalid", Purpose: "p1"}},
		{{SchoolID: "school", Access: "public", Purpose: ""}},
		{{SchoolID: "bad!school", Access: "public", Purpose: "p1"}},
		{{SchoolID: "s1", Access: "public", Purpose: "dup"}, {SchoolID: "s2", Access: "public", Purpose: "dup"}},
		{{SchoolID: "s1", Access: "public", Purpose: "p1"}, {SchoolID: "s1", Access: "public", Purpose: "p2"}},
	} {
		badCfg := h.cfg
		badCfg.UploadProfiles = badProfiles
		if _, err := r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}, badCfg); !errors.Is(err, r.ErrInput) {
			t.Fatalf("expected ErrInput for invalid UploadProfiles %+v, got %v", badProfiles, err)
		}
	}
	goodCfg := h.cfg
	goodCfg.UploadProfiles = []r.UploadProfile{
		{SchoolID: "", Access: "public", Purpose: "global-public"},
		{SchoolID: "school", Access: "restricted", Purpose: "school-restricted"},
	}
	if _, err := r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}, goodCfg); err != nil {
		t.Fatalf("valid UploadProfiles rejected: %v", err)
	}
}
