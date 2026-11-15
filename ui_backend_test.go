package resources_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
)

type countCatalog struct{ *catalog }

func (c countCatalog) CourseReferences(_ context.Context, _ r.Principal, school string, ids []string) (map[string][]string, error) {
	if school != "school" {
		return nil, r.ErrDenied
	}
	out := map[string][]string{}
	for _, id := range ids {
		if id == "other-school" {
			return nil, r.ErrDenied
		}
		if id != "course" && id != "empty" {
			return nil, r.ErrNotFound
		}
		out[id] = []string{id}
	}
	return out, nil
}
func uiResourceFixture(t *testing.T) *fixture {
	h := newFixture(t)
	var err error
	h.s, err = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: countCatalog{h.catalog}, Files: h.files, Moderation: h.moderation}, h.cfg)
	must(t, err)
	return h
}

func TestUIResourceVisibleCountsNewestTypesAndOwnerSummary(t *testing.T) {
	h := uiResourceFixture(t)
	first := h.publish(t, h.draft(t, "older", "public"))
	h.clock.Add(time.Hour)
	second := h.publish(t, h.draft(t, "newer", "restricted"))
	h.draft(t, "unfinished", "public")
	f := r.Filter{SchoolID: "school", CourseID: "course", Sort: "newest", Limit: 1, FileType: "pdf"}
	page, err := h.s.List(h.ctx, reader, f, false, false)
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].ID != second.ID || page.NextCursor == "" {
		t.Fatalf("newest page: %+v %v", page, err)
	}
	f.After = page.NextCursor
	next, err := h.s.List(h.ctx, reader, f, false, false)
	if err != nil || next.Total != 2 || len(next.Items) != 1 || next.Items[0].ID != first.ID || next.NextCursor != "" {
		t.Fatal("newest cursor", next, err)
	}
	f.Query = "changed"
	if _, err := h.s.List(h.ctx, reader, f, false, false); !errors.Is(err, r.ErrInput) {
		t.Fatal("cursor crossed filters", err)
	}
	counts, err := h.s.Counts(h.ctx, reader, r.Filter{SchoolID: "school", FileType: "pdf"}, []string{"course", "empty", "course"})
	if err != nil || counts.Total != 2 || len(counts.Courses) != 2 || counts.Courses[0].Count != 2 || counts.Courses[1].Count != 0 {
		t.Fatal("counts", counts, err)
	}
	_, err = h.s.SetState(h.ctx, manager, second.ID, "hide-newer", r.StateChange{Revision: second.Revision, State: "unlisted", Reason: "Synthetic hide"})
	must(t, err)
	counts, err = h.s.Counts(h.ctx, reader, r.Filter{SchoolID: "school", Query: "学习"}, []string{"course"})
	page, listErr := h.s.List(h.ctx, reader, r.Filter{SchoolID: "school", Query: "学习", Limit: 100}, false, false)
	if err != nil || listErr != nil || counts.Total != 1 || counts.Total != page.Total || int64(len(page.Items)) != page.Total {
		t.Fatal("visible counts/list mismatch", counts, page, err, listErr)
	}
	owned, err := h.s.OwnSummary(h.ctx, uploader)
	if err != nil || owned.ResourceCount != 3 || owned.PublishedCount != 1 {
		t.Fatal("owner summary", owned, err)
	}
	other, err := h.s.OwnSummary(h.ctx, reader)
	if err != nil || other.ResourceCount != 0 {
		t.Fatal("foreign owner count exposed", other, err)
	}
	h.auth.set("discover", true)
	if _, err := h.s.Counts(h.ctx, reader, r.Filter{SchoolID: "school"}, nil); !errors.Is(err, r.ErrDenied) {
		t.Fatal("revocation did not reject aggregate", err)
	}
	owned, err = h.s.OwnSummary(h.ctx, uploader)
	if err != nil || owned.ResourceCount != 3 {
		t.Fatal("owner stats require public admission", err)
	}
}

func TestUIResourceAggregateHTTPAuthorizationAndBoundaries(t *testing.T) {
	h := uiResourceFixture(t)
	h.publish(t, h.draft(t, "http-counts", "public"))
	handler, err := httpapi.New(h.s, authenticator{})
	must(t, err)
	for _, tc := range []struct {
		path, token string
		status      int
	}{{"/api/v1/resources/counts?school_id=school&course_ids=course,empty", "Bearer reader", 200}, {"/api/v1/resources/counts?school_id=school", "", 401}, {"/api/v1/resources/counts?school_id=other", "Bearer reader", 403}, {"/api/v1/resources/counts?school_id=school&course_ids=other-school", "Bearer reader", 403}, {"/api/v1/resources/counts?school_id=school&course_ids=", "Bearer reader", 400}, {"/api/v1/resources/counts?school_id=school&file_type=PDF", "Bearer reader", 400}, {"/api/v1/resources/counts?school_id=school&course_ids=" + strings.Repeat("course,", 100) + "course", "Bearer reader", 400}, {"/api/v1/resources/me/summary?owner=uploader", "Bearer reader", 400}, {"/api/v1/resources/me/summary", "Bearer reader", 200}, {"/api/v1/resources/me/summary", "", 401}, {"/api/v1/resources?school_id=school&sort=newest", "Bearer reader", 200}} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", tc.token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if strings.HasPrefix(tc.path, "/api/v1/resources/me/summary") && tc.status == 200 {
			var got r.OwnResourceSummary
			if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ResourceCount != 0 {
				t.Fatal("query forged owner", w.Body.String())
			}
		}
	}
}
