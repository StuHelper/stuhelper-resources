package resources_test

import (
	"context"
	"encoding/json"
	"errors"
	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"net/http/httptest"
	"strings"
	"testing"
)

func (c *catalog) ReferenceNames(_ context.Context, ids []string) (r.References, error) {
	out := r.References{Items: []r.Reference{}, MissingIDs: []string{}}
	for _, id := range ids {
		out.Items = append(out.Items, r.Reference{ID: id, Kind: "course", Name: "Name of " + id, CanonicalID: id})
	}
	return out, nil
}
func TestOwnResourceNamesRequireUploadOwnershipAndKeepContentPrivate(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "refs", "restricted"))
	h.auth.set("discover", true)
	h.auth.set("upload", true)
	handler, e := httpapi.New(h.s, authenticator{})
	must(t, e)
	call := func(path, token string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", token)
		req.Header.Set("X-User-ID", "uploader")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != status {
			t.Fatalf("%s: %d %s", path, out.Code, out.Body.String())
		}
		return out
	}
	path := "/api/v1/resources/me/references?ids=school,course"
	call(path, "", 401)
	call(path, "Bearer reader", 404)
	out := call(path, "Bearer owner", 200)
	if strings.Contains(out.Body.String(), "description") || strings.Contains(out.Body.String(), "file") {
		t.Fatal("content leaked", out.Body.String())
	}
	call("/api/v1/resources/items/"+item.ID, "Bearer reader", 403)
	call("/api/v1/resources/me/references?ids=course,unknown", "Bearer owner", 404)
	call("/api/v1/resources/me/references?ids="+strings.Repeat("course,", 100)+"course", "Bearer owner", 400)
	// A login-only complaint never grants catalog association ownership.
	_, e = h.s.CreateRequest(h.ctx, reader, item.ID, "complaint", r.Request{Kind: "complaint", Reason: "Evidence", Contact: "Contact", Evidence: []string{"Proof"}})
	must(t, e)
	call(path, "Bearer reader", 404)
}
func TestComplaintTitleSnapshotRequiresAdmissionAtSubmission(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "titles", "restricted"))
	in := r.Request{Kind: "complaint", Reason: "Evidence", Contact: "Contact", Evidence: []string{"Proof"}}
	h.auth.set("discover", true)
	without, e := h.s.CreateRequest(h.ctx, reader, item.ID, "without", in)
	must(t, e)
	if without.ResourceTitle != "" {
		t.Fatal("unadmitted title", without)
	}
	// Even an uploader's complaint follows the complaint title policy.
	own, e := h.s.CreateRequest(h.ctx, uploader, item.ID, "own-complaint", in)
	must(t, e)
	if own.ResourceTitle != "" {
		t.Fatal("own complaint bypassed admission", own)
	}
	h.auth.set("discover", false)
	retried, e := h.s.CreateRequest(h.ctx, reader, item.ID, "without", in)
	must(t, e)
	if retried.ID != without.ID || retried.ResourceTitle != "" {
		t.Fatal("retry added title", retried)
	}
	with, e := h.s.CreateRequest(h.ctx, reader, item.ID, "with", in)
	must(t, e)
	if with.ResourceTitle != item.Current.Metadata.Title {
		t.Fatal("admitted title missing", with)
	}
	// A later version must not change the saved receipt title.
	meta := item.Current.Metadata
	meta.Title = "Changed title"
	item, e = h.s.UpdateDraft(h.ctx, uploader, item.ID, "change", r.DraftWrite{Revision: item.Revision, Metadata: meta})
	must(t, e)
	h.publish(t, item)
	h.auth.set("discover", true)
	for _, v := range []r.RequestReceipt{without, with} {
		got, e := h.s.RequestStatus(h.ctx, reader, v.ID)
		must(t, e)
		if got.ResourceTitle != v.ResourceTitle {
			t.Fatal("snapshot changed", got)
		}
	}
	if _, e = h.s.RequestStatus(h.ctx, uploader, with.ID); !errors.Is(e, r.ErrNotFound) {
		t.Fatal("foreign receipt title", e)
	}
	page, e := h.s.Requests(h.ctx, reader, 100, "")
	must(t, e)
	if len(page.Items) != 2 {
		t.Fatal(page)
	}
	raw, e := json.Marshal(page)
	must(t, e)
	for _, key := range []string{"description", "file", "evidence", "Contact"} {
		if strings.Contains(string(raw), "\""+key+"\"") {
			t.Fatal("receipt exposes content", key)
		}
	}
	// Existing receipts lack an admission snapshot and must not infer a title.
	_, e = h.admin.Exec(`UPDATE resources.requests SET data=jsonb_set(data,'{Receipt}',(data->'Receipt')-'resource_title') WHERE id=$1`, with.ID)
	must(t, e)
	h.auth.set("discover", false)
	withoutSnapshot, e := h.s.RequestStatus(h.ctx, reader, with.ID)
	must(t, e)
	if withoutSnapshot.ResourceTitle != "" {
		t.Fatal("missing title fabricated", withoutSnapshot)
	}
}
