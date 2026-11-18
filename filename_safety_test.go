package resources_test

import (
	"errors"
	"fmt"
	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAttachFileRejectsUnsafeFilenamesAndStreamPreservesDisposition(t *testing.T) {
	h := newFixture(t)
	draft, e := h.s.CreateDraft(h.ctx, uploader, "fn-draft", r.DraftWrite{Metadata: metadata("public")})
	must(t, e)

	badNames := []string{
		"",
		"   ",
		".",
		"..",
		"../secret.pdf",
		"sub/lecture.pdf",
		`sub\lecture.pdf`,
		"bad\tname.pdf",
		"bad\nname.pdf",
		"bad\rname.pdf",
		"bad\x7fname.pdf",
		"bad\x00name.pdf",
		strings.Repeat("a", 252) + ".pdf",
		string([]byte{0xff, 0xfe, '.', 'p', 'd', 'f'}),
	}
	for i, bad := range badNames {
		vid := fmt.Sprintf("bad-media-%d", i)
		h.files.mu.Lock()
		h.files.items[vid] = r.FileEvidence{
			File:     r.File{VersionID: vid, Filename: bad, ContentType: "application/pdf", Size: 60, SHA256: strings.Repeat("b", 64)},
			Owner:    uploader,
			Module:   "resources",
			SchoolID: "school",
			State:    "awaiting_approval",
			Access:   "public",
		}
		h.files.mu.Unlock()
		if _, err := h.s.AttachFile(h.ctx, uploader, draft.ID, fmt.Sprintf("bad-attach-%d", i), r.FileLink{Revision: draft.Revision, MediaVersionID: vid}); !errors.Is(err, r.ErrDenied) && !errors.Is(err, r.ErrInput) {
			t.Fatalf("unsafe filename %q accepted: err=%v", bad, err)
		}
	}

	// Verify persisted draft was not modified by any rejected AttachFile call.
	unchanged, e := h.s.Get(h.ctx, uploader, draft.ID)
	must(t, e)
	if unchanged.Revision != draft.Revision || unchanged.Proposal == nil || unchanged.Proposal.File != nil {
		t.Fatalf("rejected filename mutated draft: %+v", unchanged)
	}

	// Valid multilingual / RTL filenames succeed.
	goodID := "good-rtl-media"
	h.files.mu.Lock()
	h.files.items[goodID] = r.FileEvidence{
		File:     r.File{VersionID: goodID, Filename: "محاضرات_2025.pdf", ContentType: "application/pdf", Size: 60, SHA256: strings.Repeat("c", 64)},
		Owner:    uploader,
		Module:   "resources",
		SchoolID: "school",
		State:    "awaiting_approval",
		Access:   "public",
	}
	h.files.mu.Unlock()
	attached, e := h.s.AttachFile(h.ctx, uploader, draft.ID, "good-attach", r.FileLink{Revision: draft.Revision, MediaVersionID: goodID})
	must(t, e)
	published := h.publish(t, attached)

	// Defense-in-depth: if historical DB row contains a control character in filename,
	// HTTP download/preview still emits a non-empty Content-Disposition header.
	_, e = h.admin.Exec(`UPDATE resources.versions SET data=jsonb_set(data,'{Version,file,filename}',to_jsonb($1::text)) WHERE id=$2`, "legacy\tfile.pdf", published.Current.ID)
	must(t, e)

	handler, e := httpapi.New(h.s, authenticator{})
	must(t, e)
	for _, route := range []struct {
		suffix, prefix string
	}{{"download", "attachment"}, {"preview", "inline"}} {
		req := httptest.NewRequest("GET", "/api/v1/resources/items/"+published.ID+"/"+route.suffix, nil)
		req.Header.Set("Authorization", "Bearer reader")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s status=%d", route.suffix, w.Code)
		}
		disp := w.Header().Get("Content-Disposition")
		if !strings.HasPrefix(disp, route.prefix) {
			t.Fatalf("%s lost Content-Disposition protection: %q", route.suffix, disp)
		}
	}
}
