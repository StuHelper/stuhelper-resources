package resources_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
)

func TestManagedTermsRotationPinsReceiptAndRejectsStaleOriginal(t *testing.T) {
	h := newFixture(t)
	current := managedTerms("managed-v1", "a")
	useManagedTerms(t, h, func(context.Context) (r.Terms, error) { return current, nil })
	assertTermsCapabilities(t, h, current)

	original := metadata("public")
	original.Origin = r.Origin{Kind: "original", Consent: &r.Consent{Accepted: true, Version: current.Version, Digest: current.Digest}}
	draft := createAttachedDraft(t, h, "managed-original", original)
	oldReceipt := *draft.Proposal.Metadata.Origin.Consent
	if oldReceipt.DocumentID != r.UploadLicenseDocumentID || original.Origin.Consent.DocumentID != "" {
		t.Fatalf("managed identity was not bound solely to the persisted consent: %+v", oldReceipt)
	}
	pendingBeforeChange := createAttachedDraft(t, h, "managed-pending", original)
	pendingBeforeChange, err := h.s.Submit(h.ctx, uploader, pendingBeforeChange.ID, "submit", r.Command{Revision: pendingBeforeChange.Revision})
	must(t, err)

	// The same process observes the new effective release without reconfiguration.
	current = managedTerms("managed-v2", "b")
	assertTermsCapabilities(t, h, current)
	wrongDigest := original
	wrongDigest.Origin.Consent = &r.Consent{Accepted: true, Version: current.Version, Digest: oldReceipt.Digest}
	if _, err := h.s.CreateDraft(h.ctx, uploader, "wrong-digest", r.DraftWrite{Metadata: wrongDigest}); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected a digest mismatch to be refused, got %v", err)
	}
	wrongDocument := original
	wrongDocument.Origin.Consent = &r.Consent{Accepted: true, Version: current.Version, Digest: current.Digest, DocumentID: "terms"}
	if _, err := h.s.CreateDraft(h.ctx, uploader, "wrong-document", r.DraftWrite{Metadata: wrongDocument}); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected a different document to be refused, got %v", err)
	}
	link := r.FileLink{Revision: draft.Revision, MediaVersionID: draft.Proposal.File.VersionID}
	if _, err := h.s.AttachFile(h.ctx, uploader, draft.ID, "fresh-attach", link); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected an attachment under old terms to be refused, got %v", err)
	}
	if _, err := h.s.Submit(h.ctx, uploader, draft.ID, "fresh-submit", r.Command{Revision: draft.Revision}); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected a submission under old terms to be refused, got %v", err)
	}
	stored, err := h.s.Get(h.ctx, uploader, draft.ID)
	must(t, err)
	if stored.Revision != draft.Revision || stored.Proposal.State != "draft" || *stored.Proposal.Metadata.Origin.Consent != oldReceipt {
		t.Fatal("a rejected stale operation changed the draft")
	}

	original.Origin.Consent = &r.Consent{Accepted: true, Version: current.Version, Digest: current.Digest}
	updated, err := h.s.UpdateDraft(h.ctx, uploader, draft.ID, "accept-effective", r.DraftWrite{Revision: draft.Revision, Metadata: original, Submit: true})
	must(t, err)
	assertManagedConsent(t, updated.Proposal, draft.Proposal.File, r.Consent{Accepted: true, DocumentID: current.DocumentID, Version: current.Version, Digest: current.Digest}, "pending")
	stillPending, err := h.s.Get(h.ctx, uploader, pendingBeforeChange.ID)
	must(t, err)
	assertManagedConsent(t, stillPending.Proposal, pendingBeforeChange.Proposal.File, oldReceipt, "pending")
}

func TestManagedTermsOutagePreservesReplaysAndReposts(t *testing.T) {
	h := newFixture(t)
	terms := managedTerms("managed-v1", "a")
	var sourceErr error
	lookups := 0
	useManagedTerms(t, h, func(context.Context) (r.Terms, error) {
		lookups++
		return terms, sourceErr
	})
	original := metadata("public")
	original.Origin = r.Origin{Kind: "original", Consent: &r.Consent{Accepted: true, Version: terms.Version, Digest: terms.Digest}}
	request := r.DraftWrite{Metadata: original}
	first, err := h.s.CreateDraft(h.ctx, uploader, "successful-request", request)
	must(t, err)
	published := h.publish(t, createAttachedDraft(t, h, "published-managed", original))

	sourceErr = errors.New("legal source unavailable")
	rec := termsCapabilities(t, h)
	var problem struct{ Code string }
	must(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	if rec.Code != http.StatusServiceUnavailable || problem.Code != r.ErrUnavailable.Error() {
		t.Fatalf("unexpected capabilities failure: %d %s", rec.Code, rec.Body)
	}
	before := lookups
	replay, err := h.s.CreateDraft(h.ctx, uploader, "successful-request", request)
	must(t, err)
	if replay.ID != first.ID || replay.Revision != first.Revision || *replay.Proposal.Metadata.Origin.Consent != *first.Proposal.Metadata.Origin.Consent {
		t.Fatal("an unavailable license source changed the original result")
	}
	_, err = h.s.CreateDraft(h.ctx, uploader, "repost-outage", r.DraftWrite{Metadata: metadata("public")})
	must(t, err)
	if lookups != before {
		t.Fatal("a replay or non-original upload called the license source")
	}
	beforeStats, err := h.s.OperationalStats(h.ctx)
	must(t, err)
	if _, err := h.s.CreateDraft(h.ctx, uploader, "original-outage", request); !errors.Is(err, r.ErrUnavailable) {
		t.Fatalf("expected an unavailable license to block new original consent, got %v", err)
	}
	afterStats, err := h.s.OperationalStats(h.ctx)
	must(t, err)
	if afterStats.Records != beforeStats.Records || afterStats.Operations != beforeStats.Operations || afterStats.ActiveJobs != beforeStats.ActiveJobs {
		t.Fatal("an unverified consent produced business writes or a replay receipt")
	}

	original.Title = "更正原发布标题"
	corrected, err := h.s.UpdateDraft(h.ctx, manager, published.ID, "maintain-original", r.DraftWrite{Revision: published.Revision, Metadata: original, Submit: true})
	must(t, err)
	assertManagedConsent(t, corrected.Proposal, published.Current.File, *published.Current.Metadata.Origin.Consent, "pending")
}

func managedTerms(version, digit string) r.Terms {
	return r.Terms{
		DocumentID: r.UploadLicenseDocumentID,
		Version:    version,
		Digest:     strings.Repeat(digit, 64),
		URL:        "https://example.invalid/api/v1/legal/documents/upload-license/versions/" + version + "/html",
	}
}

func useManagedTerms(t *testing.T, h *fixture, source r.TermsSource) {
	t.Helper()
	service, err := r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation, TermsSource: source}, h.cfg)
	must(t, err)
	h.s = service
}

func termsCapabilities(t *testing.T, h *fixture) *httptest.ResponseRecorder {
	t.Helper()
	handler, err := httpapi.New(h.s, authenticator{})
	must(t, err)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/resources/capabilities", nil)
	req.Header.Set("Authorization", "Bearer owner")
	rec := httptest.NewRecorder()
	contractValidatingHandler(t, handler).ServeHTTP(rec, req)
	return rec
}

func assertTermsCapabilities(t *testing.T, h *fixture, expected r.Terms) {
	t.Helper()
	rec := termsCapabilities(t, h)
	var result r.Capabilities
	must(t, json.Unmarshal(rec.Body.Bytes(), &result))
	if rec.Code != http.StatusOK || result.Terms != expected || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected license capabilities: %d %+v", rec.Code, result.Terms)
	}
}
