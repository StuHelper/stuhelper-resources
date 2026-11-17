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
