package resources_test

import (
	"bytes"
	"encoding/json"
	"errors"
	r "github.com/StuHelper/stuhelper-resources"
	"reflect"
	"testing"
)

func TestClosedUploaderKeepsFilesAndCopyrightWithoutPublicAccountLink(t *testing.T) {
	h := newFixture(t)
	item := h.draft(t, "closure", "public")
	meta := item.Proposal.Metadata
	meta.Origin = r.Origin{Kind: "known", Source: "https://source.invalid/book", Author: "Original author", License: "CC-BY-4.0"}
	item, e := h.s.UpdateDraft(h.ctx, uploader, item.ID, "provenance", r.DraftWrite{Revision: item.Revision, Metadata: meta})
	must(t, e)
	item = h.publish(t, item)
	existingDraft := h.draft(t, "closure-draft", "public")
	before, e := h.s.Get(h.ctx, reader, item.ID)
	must(t, e)
	id := "11111111-1111-4111-8111-111111111111"
	must(t, h.s.AnonymizeOwner(h.ctx, uploader, id))
	must(t, h.s.AnonymizeOwner(h.ctx, uploader, id))
	after, e := h.s.Get(h.ctx, reader, item.ID)
	must(t, e)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("public metadata changed", before, after)
	}
	raw, _ := json.Marshal(after)
	if bytes.Contains(raw, []byte(uploader.Subject)) || bytes.Contains(raw, []byte(uploader.Issuer)) {
		t.Fatal("uploader identity leak")
	}
	if len(h.files.released) != 0 {
		t.Fatal("published files released")
	}
	catalogBefore := h.catalog.calls
	if _, e = h.s.CreateDraft(h.ctx, uploader, "after-closure", r.DraftWrite{Metadata: metadata("public")}); !errors.Is(e, r.ErrDenied) {
		t.Fatal("late upload", e)
	}
	if _, e = h.s.UpdateDraft(h.ctx, uploader, item.ID, "after-closure-update", r.DraftWrite{Revision: item.Revision, Metadata: meta}); !errors.Is(e, r.ErrDenied) {
		t.Fatal("closed uploader updated published resource", e)
	}
	if _, e = h.s.UpdateDraft(h.ctx, uploader, existingDraft.ID, "after-closure-draft-update", r.DraftWrite{Revision: existingDraft.Revision, Metadata: metadata("public")}); !errors.Is(e, r.ErrDenied) {
		t.Fatal("closed uploader updated existing draft", e)
	}
	if h.catalog.calls != catalogBefore {
		t.Fatal("closed uploader triggered catalog validation", h.catalog.calls-catalogBefore)
	}
	lateFile := r.File{VersionID: "file-late-closure", Filename: "补充讲义.pdf", ContentType: "application/pdf", Size: 60, SHA256: meta.CategoryID + "000000000000000000000000000000000000000000000000000000000000"[:60]}
	h.files.mu.Lock()
	h.files.items[lateFile.VersionID] = r.FileEvidence{File: lateFile, Owner: uploader, Module: "resources", SchoolID: "school", State: "awaiting_approval", Access: "public"}
	h.files.mu.Unlock()
	if _, e = h.s.AttachFile(h.ctx, uploader, existingDraft.ID, "after-closure-attach", r.FileLink{Revision: existingDraft.Revision, MediaVersionID: lateFile.VersionID}); !errors.Is(e, r.ErrDenied) {
		t.Fatal("closed uploader attached file to existing draft", e)
	}
	if _, e = h.s.Submit(h.ctx, uploader, existingDraft.ID, "after-closure-submit", r.Command{Revision: existingDraft.Revision}); !errors.Is(e, r.ErrDenied) {
		t.Fatal("closed uploader submitted existing draft", e)
	}
	stillUnchanged, e := h.s.Get(h.ctx, reader, item.ID)
	must(t, e)
	if stillUnchanged.Revision != item.Revision {
		t.Fatal("closed uploader mutated item revision", stillUnchanged.Revision, item.Revision)
	}
	draftCheck, e := h.s.Get(h.ctx, manager, existingDraft.ID)
	must(t, e)
	if draftCheck.Revision != existingDraft.Revision || draftCheck.Proposal == nil || draftCheck.Proposal.State != "draft" {
		t.Fatal("closed uploader mutated existing draft", draftCheck)
	}
	// Administrators retain management actions on historical records of closed owners.
	unlisted, e := h.s.SetState(h.ctx, manager, item.ID, "admin-unlist-closed", r.StateChange{Revision: item.Revision, State: "unlisted", Reason: "管理维护"})
	must(t, e)
	if unlisted.State != "unlisted" {
		t.Fatal("admin state change failed on closed owner record", unlisted)
	}
	if e = h.s.AnonymizeOwner(h.ctx, uploader, "22222222-2222-4222-8222-222222222222"); !errors.Is(e, r.ErrConflict) {
		t.Fatal(e)
	}
}
