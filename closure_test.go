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
	if _, e = h.s.CreateDraft(h.ctx, uploader, "after-closure", r.DraftWrite{Metadata: metadata("public")}); !errors.Is(e, r.ErrDenied) {
		t.Fatal("late upload", e)
	}
	if e = h.s.AnonymizeOwner(h.ctx, uploader, "22222222-2222-4222-8222-222222222222"); !errors.Is(e, r.ErrConflict) {
		t.Fatal(e)
	}
}
