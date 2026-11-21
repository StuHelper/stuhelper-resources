package resources_test

import (
	"errors"
	"strings"
	"testing"

	r "github.com/StuHelper/stuhelper-resources"
)

func TestManagerMaintainsPublishedMetadataWithoutReplacingConsent(t *testing.T) {
	h := newFixture(t)
	old := r.Consent{Accepted: true, Version: h.cfg.Terms.Version, Digest: h.cfg.Terms.Digest}
	original := metadata("public")
	original.Origin = r.Origin{Kind: "original", Author: "作者", Consent: &old}
	draft := createAttachedDraft(t, h, "published-original", original)
	published := h.publish(t, draft)
	rotateTerms(t, h)
	current := r.Consent{Accepted: true, Version: h.cfg.Terms.Version, Digest: h.cfg.Terms.Digest}
	for key, attempt := range map[string]func(*r.DraftWrite){
		"replace-consent": func(in *r.DraftWrite) { in.Metadata.Origin.Consent = &current },
		"replace-author":  func(in *r.DraftWrite) { in.Metadata.Origin.Author = "管理者" },
		"erase-original":  func(in *r.DraftWrite) { in.Metadata.Origin = r.Origin{Kind: "unknown"} },
		"replace-file":    func(in *r.DraftWrite) { in.MediaVersionID = "manager-file" },
		"change-access":   func(in *r.DraftWrite) { in.Metadata.Access = "restricted" },
		"grant-badge":     func(in *r.DraftWrite) { in.Metadata.Origin.VerifiedInstructor = true },
	} {
		t.Run(key, func(t *testing.T) {
			in := r.DraftWrite{Revision: published.Revision, Metadata: original}
			attempt(&in)
			if _, err := h.s.UpdateDraft(h.ctx, manager, published.ID, key, in); !errors.Is(err, r.ErrDenied) {
				t.Fatalf("expected refusal of manager %s, got %v", key, err)
			}
		})
	}
	unchanged, err := h.s.Get(h.ctx, uploader, published.ID)
	must(t, err)
	if unchanged.Revision != published.Revision || unchanged.Proposal != nil || unchanged.Current == nil || *unchanged.Current.Metadata.Origin.Consent != old {
		t.Fatal("denied manager writes changed the published record")
	}

	edited := original
	edited.Title = "更正后的标题"
	update := r.DraftWrite{Revision: published.Revision, Metadata: edited}
	proposal, err := h.s.UpdateDraft(h.ctx, manager, published.ID, "correct-title", update)
	must(t, err)
	assertManagedConsent(t, proposal.Proposal, published.Current.File, old, "draft")
	replay, err := h.s.UpdateDraft(h.ctx, manager, published.ID, "correct-title", update)
	must(t, err)
	if replay.Proposal == nil || replay.Proposal.ID != proposal.Proposal.ID || replay.Revision != proposal.Revision {
		t.Fatal("a metadata correction did not replay its original result")
	}
	if _, err = h.s.AttachFile(h.ctx, manager, published.ID, "replace-through-file", r.FileLink{Revision: proposal.Revision, MediaVersionID: "manager-file"}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected file attachment by manager to be refused, got %v", err)
	}
	pending, err := h.s.Submit(h.ctx, manager, published.ID, "submit-correction", r.Command{Revision: proposal.Revision})
	must(t, err)
	assertManagedConsent(t, pending.Proposal, published.Current.File, old, "pending")
	h.drain(t)
	_, err = h.s.ApplyDecision(h.ctx, manager, published.ID, "approve-correction", r.Decision{ID: "approval-correction", Revision: pending.Revision, VersionID: pending.Proposal.ID, Action: "approve"})
	must(t, err)
	h.drain(t)
	revised, err := h.s.Get(h.ctx, uploader, published.ID)
	must(t, err)
	if revised.Proposal != nil || revised.Current == nil || revised.Current.Metadata.Title != edited.Title ||
		*revised.Current.Metadata.Origin.Consent != old || *revised.Current.File != *published.Current.File {
		t.Fatal("approved metadata correction lost the original work or consent")
	}
	edited.Title = "再次更正标题"
	direct, err := h.s.UpdateDraft(h.ctx, manager, published.ID, "direct-correction", r.DraftWrite{Revision: revised.Revision, Metadata: edited, Submit: true})
	must(t, err)
	assertManagedConsent(t, direct.Proposal, revised.Current.File, old, "pending")
}

func TestManagerCannotGrantConsentOrSubmitUnpublishedStaleDraft(t *testing.T) {
	h := newFixture(t)
	old := r.Consent{Accepted: true, Version: h.cfg.Terms.Version, Digest: h.cfg.Terms.Digest}
	original := metadata("public")
	original.Origin = r.Origin{Kind: "original", Consent: &old}
	draft := createAttachedDraft(t, h, "unpublished-original", original)
	repost := createAttachedDraft(t, h, "unpublished-repost", metadata("public"))
	published := h.publish(t, createAttachedDraft(t, h, "another-published-original", original))
	replacement, err := h.s.UpdateDraft(h.ctx, uploader, published.ID, "replace-before-rotation", r.DraftWrite{
		Revision: published.Revision, Metadata: original, MediaVersionID: draft.Proposal.File.VersionID,
	})
	must(t, err)
	toReject := createAttachedDraft(t, h, "another-unpublished-original", original)
	pending, err := h.s.Submit(h.ctx, uploader, toReject.ID, "submit-before-rotation", r.Command{Revision: toReject.Revision})
	must(t, err)
	h.drain(t)
	rejected, err := h.s.ApplyDecision(h.ctx, manager, pending.ID, "reject-before-rotation", r.Decision{
		ID: "reject-old-proposal", Revision: pending.Revision, VersionID: pending.Proposal.ID, Action: "reject", Reason: "请更正标题",
	})
	must(t, err)
	rotateTerms(t, h)
	current := r.Consent{Accepted: true, Version: h.cfg.Terms.Version, Digest: h.cfg.Terms.Digest}
	newOriginal := original
	newOriginal.Origin.Consent = &current

	_, err = h.s.UpdateDraft(h.ctx, manager, repost.ID, "manager-conversion", r.DraftWrite{Revision: repost.Revision, Metadata: newOriginal, Submit: true})
	if !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected manager conversion to original to be refused, got %v", err)
	}
	if _, err = h.s.UpdateDraft(h.ctx, manager, draft.ID, "manager-current-consent", r.DraftWrite{Revision: draft.Revision, Metadata: newOriginal}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected manager replacing draft consent to be refused, got %v", err)
	}
	original.Title = "管理侧修正草稿标题"
	if _, err = h.s.UpdateDraft(h.ctx, manager, draft.ID, "manager-direct-submission", r.DraftWrite{Revision: draft.Revision, Metadata: original, Submit: true}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected manager direct stale submission to be refused, got %v", err)
	}
	corrected, err := h.s.UpdateDraft(h.ctx, manager, draft.ID, "manager-title", r.DraftWrite{Revision: draft.Revision, Metadata: original})
	must(t, err)
	assertManagedConsent(t, corrected.Proposal, draft.Proposal.File, old, "draft")
	if _, err = h.s.Submit(h.ctx, manager, corrected.ID, "manager-submit-legacy", r.Command{Revision: corrected.Revision}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected manager stale submission after metadata edit to be refused, got %v", err)
	}
	if _, err = h.s.Submit(h.ctx, manager, replacement.ID, "manager-submit-replacement", r.Command{Revision: replacement.Revision}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected manager to be refused when replacing the old work with a different media version of the same digest, got %v", err)
	}
	unchanged, err := h.s.Get(h.ctx, uploader, corrected.ID)
	must(t, err)
	assertManagedConsent(t, unchanged.Proposal, draft.Proposal.File, old, "draft")
	if unchanged.Revision != corrected.Revision {
		t.Fatal("denied submission updated the revision")
	}
	fixedTitle, err := h.s.UpdateDraft(h.ctx, manager, rejected.ID, "correct-rejected-title", r.DraftWrite{Revision: rejected.Revision, Metadata: original})
	must(t, err)
	assertManagedConsent(t, fixedTitle.Proposal, rejected.Proposal.File, old, "draft")
	if _, err = h.s.Submit(h.ctx, manager, fixedTitle.ID, "manager-submit-rejected-correction", r.Command{Revision: fixedTitle.Revision}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected stale rejected draft correction to require the owner's current consent before submission, got %v", err)
	}

	if _, err = h.s.UpdateDraft(h.ctx, uploader, repost.ID, "owner-stale", r.DraftWrite{Revision: repost.Revision, Metadata: original, Submit: true}); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected owner conversion with stale terms to be refused, got %v", err)
	}
	converted, err := h.s.UpdateDraft(h.ctx, uploader, repost.ID, "owner-current", r.DraftWrite{Revision: repost.Revision, Metadata: newOriginal, Submit: true})
	must(t, err)
	assertManagedConsent(t, converted.Proposal, repost.Proposal.File, current, "pending")
}

func TestManagerMetadataEditDoesNotResubmitInstructorClaim(t *testing.T) {
	h := newFixture(t)
	teacherKey := uploader.Key() + "|school|course"
	ic := &instructorAliasCatalog{catalog: h.catalog, allowed: map[string]bool{teacherKey: true}}
	var err error
	h.s, err = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: ic, Files: h.files, Moderation: h.moderation}, h.cfg)
	must(t, err)
	m := metadata("public")
	m.Origin.VerifiedInstructor = true
	published := h.publish(t, createAttachedDraft(t, h, "published-instructor", m))
	delete(ic.allowed, teacherKey)

	changedCourse := m
	changedCourse.Associations.CourseID = "legacy-course"
	if _, err = h.s.UpdateDraft(h.ctx, manager, published.ID, "move-course", r.DraftWrite{Revision: published.Revision, Metadata: changedCourse}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected the manager to be unable to move the instructor claim to another course, got %v", err)
	}
	m.Title = "任课资料更正"
	if _, err = h.s.UpdateDraft(h.ctx, manager, published.ID, "submit-title", r.DraftWrite{Revision: published.Revision, Metadata: m, Submit: true}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected the manager to be unable to resubmit the instructor claim, got %v", err)
	}
	draft, err := h.s.UpdateDraft(h.ctx, manager, published.ID, "save-title", r.DraftWrite{Revision: published.Revision, Metadata: m})
	must(t, err)
	if draft.Proposal == nil || !draft.Proposal.Metadata.Origin.VerifiedInstructor || draft.Proposal.VerifiedInstructor {
		t.Fatal("saving metadata did not preserve the declaration separately from the verified snapshot")
	}
	if _, err = h.s.Submit(h.ctx, manager, draft.ID, "manager-submit", r.Command{Revision: draft.Revision}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected the manager to be unable to submit the instructor draft, got %v", err)
	}
	if _, err = h.s.Submit(h.ctx, uploader, draft.ID, "owner-submit", r.Command{Revision: draft.Revision}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected the owner to require current instructor qualification to submit, got %v", err)
	}
}

func createAttachedDraft(t *testing.T, h *fixture, key string, m r.Metadata) r.Resource {
	t.Helper()
	draft, err := h.s.CreateDraft(h.ctx, uploader, key, r.DraftWrite{Metadata: m})
	must(t, err)
	file := r.File{VersionID: "media-" + key, Filename: "笔记.pdf", ContentType: "application/pdf", Size: 60, SHA256: strings.Repeat("e", 64)}
	h.files.items[file.VersionID] = r.FileEvidence{File: file, Owner: uploader, Module: "resources", SchoolID: "school", State: "awaiting_approval", Access: m.Access}
	result, err := h.s.AttachFile(h.ctx, uploader, draft.ID, "link", r.FileLink{Revision: draft.Revision, MediaVersionID: file.VersionID})
	must(t, err)
	return result
}

func rotateTerms(t *testing.T, h *fixture) {
	t.Helper()
	h.cfg.Terms.Version = "fixture-v2"
	h.cfg.Terms.Digest = strings.Repeat("f", 64)
	var err error
	h.s, err = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}, h.cfg)
	must(t, err)
}

func assertManagedConsent(t *testing.T, v *r.Version, file *r.File, consent r.Consent, state string) {
	t.Helper()
	if v == nil || v.State != state || v.File == nil || *v.File != *file ||
		v.Metadata.Origin.Consent == nil || *v.Metadata.Origin.Consent != consent {
		t.Fatalf("expected %s with the original file and consent, got %+v", state, v)
	}
}
