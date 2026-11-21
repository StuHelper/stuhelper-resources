package resources_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"github.com/StuHelper/stuhelper-resources/sdk"
)

func TestTermsRotationRequiresCurrentOriginalConsentForDraftSubmission(t *testing.T) {
	h := newFixture(t)
	metadata := metadata("public")
	oldConsent := r.Consent{Accepted: true, Version: h.cfg.Terms.Version, Digest: h.cfg.Terms.Digest}
	metadata.Origin = r.Origin{Kind: "original", Consent: &oldConsent}
	original := r.DraftWrite{Metadata: metadata}
	fileless, err := h.s.CreateDraft(h.ctx, uploader, "old-fileless", original)
	must(t, err)
	file := r.File{VersionID: "file-original", Filename: "笔记.pdf", ContentType: "application/pdf", Size: 60, SHA256: strings.Repeat("c", 64)}
	h.files.items[file.VersionID] = r.FileEvidence{File: file, Owner: uploader, Module: "resources", SchoolID: "school", State: "awaiting_approval", Access: "public"}
	newOldDraftWithFile := func(key string) (r.Resource, r.FileLink) {
		t.Helper()
		draft, err := h.s.CreateDraft(h.ctx, uploader, key, original)
		must(t, err)
		link := r.FileLink{Revision: draft.Revision, MediaVersionID: file.VersionID}
		attached, err := h.s.AttachFile(h.ctx, uploader, draft.ID, "attach", link)
		must(t, err)
		return attached, link
	}
	staleAttached, priorLink := newOldDraftWithFile("old-attached")
	toSubmit, _ := newOldDraftWithFile("old-submitted")
	priorSubmit := r.Command{Revision: toSubmit.Revision}
	submitted, err := h.s.Submit(h.ctx, uploader, toSubmit.ID, "submit", priorSubmit)
	must(t, err)
	oldCounts, err := h.s.OperationalStats(h.ctx)
	must(t, err)

	h.cfg.Terms.Version = "fixture-v2"
	h.cfg.Terms.Digest = strings.Repeat("d", 64)
	h.s, err = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}, h.cfg)
	must(t, err)
	link := r.FileLink{Revision: fileless.Revision, MediaVersionID: file.VersionID}
	if _, err = h.s.AttachFile(h.ctx, uploader, fileless.ID, "attach-current", link); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected to refuse a new file under stale terms, got %v", err)
	}
	command := r.Command{Revision: staleAttached.Revision}
	if _, err = h.s.Submit(h.ctx, uploader, staleAttached.ID, "submit-current", command); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected to refuse first submission under stale terms, got %v", err)
	}
	if _, err = h.s.CreateDraft(h.ctx, uploader, "fresh-with-stale-terms", original); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected to refuse a new draft under stale terms, got %v", err)
	}

	// An unknown outcome can be recovered without submitting a new operation.
	replay, err := h.s.CreateDraft(h.ctx, uploader, "old-fileless", original)
	must(t, err)
	if replay.ID != fileless.ID || replay.Revision != fileless.Revision || replay.Proposal.File != nil || *replay.Proposal.Metadata.Origin.Consent != oldConsent {
		t.Fatal("old create result changed")
	}
	replay, err = h.s.AttachFile(h.ctx, uploader, staleAttached.ID, "attach", priorLink)
	must(t, err)
	if replay.Revision != staleAttached.Revision || replay.Proposal.File.VersionID != file.VersionID || *replay.Proposal.Metadata.Origin.Consent != oldConsent {
		t.Fatal("old attachment result changed")
	}
	replay, err = h.s.Submit(h.ctx, uploader, submitted.ID, "submit", priorSubmit)
	must(t, err)
	if replay.Revision != submitted.Revision || replay.Proposal.State != "pending" || *replay.Proposal.Metadata.Origin.Consent != oldConsent {
		t.Fatal("old submission result changed")
	}
	currentFileless, err := h.s.Get(h.ctx, uploader, fileless.ID)
	must(t, err)
	if currentFileless.Revision != fileless.Revision || currentFileless.Proposal.File != nil || *currentFileless.Proposal.Metadata.Origin.Consent != oldConsent {
		t.Fatal("refused attachment changed the stored draft")
	}
	currentAttached, err := h.s.Get(h.ctx, uploader, staleAttached.ID)
	must(t, err)
	if currentAttached.Revision != staleAttached.Revision || currentAttached.Proposal.State != "draft" || *currentAttached.Proposal.Metadata.Origin.Consent != oldConsent {
		t.Fatal("refused submission changed the stored draft")
	}
	counts, err := h.s.OperationalStats(h.ctx)
	must(t, err)
	if counts.Records != oldCounts.Records || counts.Operations != oldCounts.Operations || counts.ActiveJobs != oldCounts.ActiveJobs {
		t.Fatal("refused and replayed writes changed resource, replay or job counts")
	}

	// A review started before the change still uses the original consent.
	h.drain(t)
	_, err = h.s.ApplyDecision(h.ctx, manager, submitted.ID, "approve", r.Decision{ID: "approve-old", Revision: submitted.Revision, VersionID: submitted.Proposal.ID, Action: "approve"})
	must(t, err)
	h.drain(t)
	published, err := h.s.Get(h.ctx, uploader, submitted.ID)
	must(t, err)
	if published.Current == nil || *published.Current.Metadata.Origin.Consent != oldConsent {
		t.Fatal("publication replaced historical consent")
	}

	currentConsent := r.Consent{Accepted: true, Version: h.cfg.Terms.Version, Digest: h.cfg.Terms.Digest}
	metadata.Origin.Consent = &currentConsent
	updated, err := h.s.UpdateDraft(h.ctx, uploader, fileless.ID, "accept-current", r.DraftWrite{Revision: fileless.Revision, Metadata: metadata})
	must(t, err)
	link.Revision = updated.Revision
	attached, err := h.s.AttachFile(h.ctx, uploader, updated.ID, "attach-current", link)
	must(t, err)
	pending, err := h.s.Submit(h.ctx, uploader, attached.ID, "submit-current", r.Command{Revision: attached.Revision})
	must(t, err)
	if pending.Proposal.State != "pending" || *pending.Proposal.Metadata.Origin.Consent != currentConsent {
		t.Fatal("new submission did not preserve current consent")
	}
}

func TestDraftOneStepSubmitRejectionReasonWithdrawAndDelete(t *testing.T) {
	h := newFixture(t)
	handler, err := httpapi.New(h.s, authenticator{})
	must(t, err)
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := sdk.NewClientWithResponses(server.URL)
	must(t, err)

	seedMedia := func(id, access string) {
		file := r.File{VersionID: id, Filename: id + ".pdf", ContentType: "application/pdf", Size: 60, SHA256: strings.Repeat("c", 64)}
		h.files.mu.Lock()
		h.files.items[id] = r.FileEvidence{File: file, Owner: uploader, Module: "resources", SchoolID: "school", State: "awaiting_approval", Access: access}
		h.files.mu.Unlock()
	}

	// Submit=true without media_version_id is rejected before calling external catalog/files.
	beforeCatalog := h.catalog.calls
	if _, err := h.s.CreateDraft(h.ctx, uploader, "missing-file-submit", r.DraftWrite{Metadata: metadata("public"), Submit: true}); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected ErrInput, got %v", err)
	}
	if h.catalog.calls != beforeCatalog {
		t.Fatal("invalid one-step submit reached catalog")
	}

	// One-step CreateDraft + file attach + submit via generated SDK.
	seedMedia("file-onestep-1", "public")
	mediaID := "file-onestep-1"
	submitFlag := true
	createParams := &sdk.CreateDraftParams{IdempotencyKey: "sdk-onestep-create"}
	meta := metadata("public")
	year := int64(2025)
	sdkCreate, err := client.CreateDraftWithResponse(h.ctx, createParams, sdk.CreateDraftJSONRequestBody{
		Revision:       0,
		MediaVersionId: &mediaID,
		Submit:         &submitFlag,
		Metadata: sdk.Metadata{
			Title:            meta.Title,
			Description:      meta.Description,
			Tags:             meta.Tags,
			CategoryId:       meta.CategoryID,
			CategoryRevision: meta.CategoryRevision,
			Attributes:       map[string]sdk.Value{"year": {Number: &year}},
			Associations:     sdk.Associations{SchoolId: "school", CourseId: &meta.Associations.CourseID},
			Access:           sdk.MetadataAccess("public"),
			Origin:           sdk.Origin{Kind: sdk.OriginKind("unknown")},
		},
	}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer owner")
		return nil
	})
	must(t, err)
	if sdkCreate.StatusCode() != 200 || sdkCreate.JSON200 == nil || sdkCreate.JSON200.Proposal == nil || sdkCreate.JSON200.Proposal.State != sdk.VersionState("pending") || sdkCreate.JSON200.Proposal.SubmittedAt == nil || sdkCreate.JSON200.Proposal.File == nil {
		t.Fatalf("one-step SDK create failed: status=%d body=%s", sdkCreate.StatusCode(), sdkCreate.Body)
	}
	itemID := sdkCreate.JSON200.Id
	staleVersionID := sdkCreate.JSON200.Proposal.Id
	staleRevision := sdkCreate.JSON200.Revision
	h.drain(t)

	// Cannot delete while pending; non-owner cannot withdraw.
	if _, err := h.s.DeleteDraft(h.ctx, uploader, itemID, "del-pending", staleRevision); !errors.Is(err, r.ErrConflict) {
		t.Fatalf("pending draft deleted: %v", err)
	}
	if _, err := h.s.WithdrawProposal(h.ctx, reader, itemID, "foreign-withdraw", r.Command{Revision: staleRevision}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("foreign user withdrew proposal: %v", err)
	}

	// Withdraw pending proposal via SDK; late moderation decision on old version is rejected.
	withdrawResp, err := client.WithdrawProposalWithResponse(h.ctx, itemID, &sdk.WithdrawProposalParams{IdempotencyKey: "sdk-withdraw-1"}, sdk.Command{Revision: staleRevision}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer owner")
		return nil
	})
	must(t, err)
	if withdrawResp.StatusCode() != 200 || withdrawResp.JSON200 == nil || withdrawResp.JSON200.Proposal == nil || withdrawResp.JSON200.Proposal.State != sdk.VersionState("draft") || withdrawResp.JSON200.Proposal.File == nil {
		t.Fatalf("withdraw failed: status=%d body=%s", withdrawResp.StatusCode(), withdrawResp.Body)
	}
	if _, err := h.s.ApplyDecision(h.ctx, manager, itemID, "late-approve", r.Decision{ID: "dec-stale", Revision: staleRevision, VersionID: staleVersionID, Action: "approve"}); !errors.Is(err, r.ErrConflict) {
		t.Fatalf("stale review accepted after withdraw: %v", err)
	}

	// Re-submit via one-step UpdateDraft, then reject with reason containing control char -> sanitized reason visible to owner, hidden once published.
	updated, err := h.s.UpdateDraft(h.ctx, uploader, itemID, "resubmit-update", r.DraftWrite{
		Revision: withdrawResp.JSON200.Revision,
		Metadata: meta,
		Submit:   true,
	})
	must(t, err)
	if updated.Proposal == nil || updated.Proposal.State != "pending" || updated.Proposal.SubmittedAt == nil {
		t.Fatalf("one-step update submit failed: %+v", updated)
	}
	h.drain(t)
	rejected, err := h.s.ApplyDecision(h.ctx, manager, itemID, "reject-1", r.Decision{
		ID:        "dec-reject-1",
		Revision:  updated.Revision,
		VersionID: updated.Proposal.ID,
		Action:    "reject",
		Reason:    "分类选错\x07，请改为讲义分类",
	})
	must(t, err)
	if rejected.Proposal == nil || rejected.Proposal.State != "rejected" || rejected.Proposal.DecisionReason != "分类选错，请改为讲义分类" {
		t.Fatalf("decision_reason missing or unsanitized: %+v", rejected.Proposal)
	}
	ownFetched, err := h.s.Get(h.ctx, uploader, itemID)
	must(t, err)
	if ownFetched.Proposal == nil || ownFetched.Proposal.DecisionReason != "分类选错，请改为讲义分类" {
		t.Fatalf("owner could not read rejection reason: %+v", ownFetched.Proposal)
	}

	// Delete the never-published rejected draft via SDK, verify idempotent replay, conflict on changed revision, 404 on new key, and summary count update.
	summaryBefore, err := h.s.OwnSummary(h.ctx, uploader)
	must(t, err)
	if summaryBefore.ResourceCount != 1 {
		t.Fatalf("expected 1 own resource before delete, got %+v", summaryBefore)
	}
	if _, err := h.s.DeleteDraft(h.ctx, reader, itemID, "foreign-delete", 0); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("foreign user deleted draft: %v", err)
	}
	rev := rejected.Revision
	// Cleanup for a superseded version can still be waiting when the
	// never-published draft becomes eligible for deletion.
	must(t, h.repo.Transaction(h.ctx, func(tx r.Transaction) error {
		return tx.SaveJob(r.Job{ID: "cleanup-before-delete", Kind: "release", ResourceID: itemID, VersionID: updated.Proposal.ID, ReferenceID: "superseded-ref", State: "pending", Generation: 1, AvailableAt: h.clock.Now()})
	}))
	if _, err := h.s.DeleteDraft(h.ctx, uploader, itemID, "sdk-del-1", rev); !errors.Is(err, r.ErrConflict) {
		t.Fatalf("expected deletion to wait for release, got %v", err)
	}
	_, err = h.s.Get(h.ctx, uploader, itemID)
	must(t, err)
	h.drain(t)
	if !h.files.released["superseded-ref"] {
		t.Fatal("release job lost before provider confirmation")
	}

	delResp, err := client.DeleteDraftWithResponse(h.ctx, itemID, &sdk.DeleteDraftParams{IdempotencyKey: "sdk-del-1", Revision: &rev}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer owner")
		return nil
	})
	must(t, err)
	if delResp.StatusCode() != 200 || delResp.JSON200 == nil || !delResp.JSON200.Deleted || delResp.JSON200.Id != itemID {
		t.Fatalf("delete failed: status=%d body=%s", delResp.StatusCode(), delResp.Body)
	}
	replayResp, err := client.DeleteDraftWithResponse(h.ctx, itemID, &sdk.DeleteDraftParams{IdempotencyKey: "sdk-del-1", Revision: &rev}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer owner")
		return nil
	})
	must(t, err)
	if replayResp.StatusCode() != 200 || replayResp.JSON200 == nil || !replayResp.JSON200.Deleted {
		t.Fatalf("delete idempotent replay failed: status=%d body=%s", replayResp.StatusCode(), replayResp.Body)
	}
	otherRev := rev + 1
	conflictResp, err := client.DeleteDraftWithResponse(h.ctx, itemID, &sdk.DeleteDraftParams{IdempotencyKey: "sdk-del-1", Revision: &otherRev}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer owner")
		return nil
	})
	must(t, err)
	if conflictResp.StatusCode() != 409 {
		t.Fatalf("expected 409 on changed delete payload, got %d", conflictResp.StatusCode())
	}
	notFoundResp, err := client.DeleteDraftWithResponse(h.ctx, itemID, &sdk.DeleteDraftParams{IdempotencyKey: "sdk-del-2"}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer owner")
		return nil
	})
	must(t, err)
	if notFoundResp.StatusCode() != 404 {
		t.Fatalf("expected 404 on second delete key, got %d", notFoundResp.StatusCode())
	}
	summaryAfter, err := h.s.OwnSummary(h.ctx, uploader)
	must(t, err)
	if summaryAfter.ResourceCount != 0 {
		t.Fatalf("deleted draft remained in own summary: %+v", summaryAfter)
	}

	// Published resource cannot be deleted via DeleteDraft, and Current strips decision_reason / submitted_at.
	seedMedia("file-pub-1", "public")
	pubDraft, err := h.s.CreateDraft(h.ctx, uploader, "pub-onestep", r.DraftWrite{Metadata: meta, MediaVersionID: "file-pub-1", Submit: true})
	must(t, err)
	h.drain(t)
	_, err = h.s.ApplyContentDecision(h.ctx, manager, r.Decision{ID: "mod-pub-1", Revision: pubDraft.Revision, VersionID: pubDraft.Proposal.ID, Action: "publish", Reason: "内部审核备注"})
	must(t, err)
	h.drain(t)
	publicView, err := h.s.Get(h.ctx, reader, pubDraft.ID)
	must(t, err)
	if publicView.Current == nil || publicView.Current.DecisionReason != "" || publicView.Current.SubmittedAt != nil || publicView.Proposal != nil {
		t.Fatalf("public view leaked proposal fields: %+v", publicView)
	}
	if _, err := h.s.DeleteDraft(h.ctx, uploader, pubDraft.ID, "del-published", 0); !errors.Is(err, r.ErrConflict) {
		t.Fatalf("published resource allowed DeleteDraft: %v", err)
	}
}
