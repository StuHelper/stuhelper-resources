package resources_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/contracts"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"github.com/StuHelper/stuhelper-resources/sdk"
	"github.com/getkin/kin-openapi/openapi3"
)

type countCatalog struct{ *catalog }

func (c countCatalog) Validate(ctx context.Context, a r.Associations) error {
	if err := c.catalog.Validate(ctx, a); err != nil {
		return err
	}
	if a.TeacherID == "other-school-teacher" {
		return r.ErrDenied
	}
	if a.TeacherID != "" && a.TeacherID != "teacher-a" && a.TeacherID != "teacher-b" {
		return r.ErrNotFound
	}
	if a.TermID != "" && a.TermID != "2024-fall" && a.TermID != "2025-spring" {
		return r.ErrNotFound
	}
	return nil
}

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

func TestResourceListCursorKeepsExistingLengthLimit(t *testing.T) {
	h := uiResourceFixture(t)
	first := h.publish(t, h.draft(t, "cursor-older", "public"))
	h.clock.Add(time.Hour)
	second := h.publish(t, h.draft(t, "cursor-newer", "public"))
	handler, err := httpapi.New(h.s, authenticator{})
	must(t, err)
	for _, tc := range []struct{ path, token string }{
		{"/api/v1/resources", "Bearer reader"},
		{"/api/v1/resources/manage", "Bearer global"},
		{"/api/v1/resources/me", "Bearer owner"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			query := url.Values{"school_id": {"school"}, "sort": {"newest"}, "limit": {"1"}}
			get := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest("GET", tc.path+"?"+query.Encode(), nil)
				req.Header.Set("Authorization", tc.token)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				return w
			}
			w := get()
			var page r.Page
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != second.ID || page.NextCursor == "" {
				t.Fatalf("first page: %d %s", w.Code, w.Body.String())
			}
			data, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
			must(t, err)
			// JSON whitespace preserves the cursor scope and position while exercising the
			// existing 2000-character HTTP limit with a valid newest cursor.
			data = append(data, []byte(strings.Repeat(" ", 1500-len(data)))...)
			query.Set("after", base64.RawURLEncoding.EncodeToString(data))
			w = get()
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != first.ID || page.NextCursor != "" || page.Total != 2 {
				t.Fatalf("2000-character cursor: %d %s", w.Code, w.Body.String())
			}
			query.Set("after", base64.RawURLEncoding.EncodeToString(append(data, ' ')))
			if w = get(); w.Code != 400 {
				t.Fatalf("over-limit cursor: %d", w.Code)
			}
		})
	}
}

func TestResourceBrowseFiltersAndDynamicGroupFacets(t *testing.T) {
	h := uiResourceFixture(t)
	_, err := h.s.PutCategory(h.ctx, global, "slides", "seed-slides", r.CategoryWrite{
		Names:   r.Names{"zh-CN": "课程讲义"},
		Enabled: true,
		Order:   15,
		Fields: []r.Field{{
			Key: "stage", Names: r.Names{"zh-CN": "阶段"}, Kind: "choice", Filterable: true, Groupable: true,
			Options: []r.Option{
				{ID: "final", Names: r.Names{"zh-CN": "期末"}},
				{ID: "midterm", Names: r.Names{"zh-CN": "期中"}},
			},
		}},
		GroupBy: "stage",
	})
	must(t, err)
	_, err = h.s.PutCategory(h.ctx, global, "plain", "seed-plain", r.CategoryWrite{
		Names: r.Names{"zh-CN": "参考书"}, Enabled: true, Order: 25, Fields: []r.Field{},
	})
	must(t, err)

	makePub := func(key, cat string, attrs map[string]r.Value, teacher, term, access string, tags []string) r.Resource {
		t.Helper()
		meta := r.Metadata{
			Title: "高等数学-" + key, Description: "课程资料", Tags: tags,
			CategoryID: cat, CategoryRevision: 1, Attributes: attrs,
			Associations: r.Associations{SchoolID: "school", CourseID: "course", TeacherID: teacher, TermID: term},
			Access:       access, Origin: r.Origin{Kind: "unknown"},
		}
		item, err := h.s.CreateDraft(h.ctx, uploader, "draft-"+key, r.DraftWrite{Metadata: meta})
		must(t, err)
		file := r.File{VersionID: "file-" + key, Filename: key + ".pdf", ContentType: "application/pdf", Size: 60, SHA256: strings.Repeat("b", 64)}
		h.files.mu.Lock()
		h.files.items[file.VersionID] = r.FileEvidence{File: file, Owner: uploader, Module: "resources", SchoolID: "school", State: "awaiting_approval", Access: access}
		h.files.mu.Unlock()
		item, err = h.s.AttachFile(h.ctx, uploader, item.ID, "attach-"+key, r.FileLink{Revision: item.Revision, MediaVersionID: file.VersionID})
		must(t, err)
		return h.publish(t, item)
	}

	y2024, y2025 := int64(2024), int64(2025)
	optMid, optFinal := "midterm", "final"
	r1 := makePub("r1", "exam", map[string]r.Value{"year": {Number: &y2025}}, "teacher-a", "2025-spring", "public", []string{"真题", "解析"})
	h.clock.Add(time.Minute)
	r2 := makePub("r2", "exam", map[string]r.Value{"year": {Number: &y2024}}, "teacher-b", "2024-fall", "restricted", []string{"真题"})
	h.clock.Add(time.Minute)
	makePub("r3", "exam", map[string]r.Value{}, "teacher-a", "2025-spring", "public", []string{"汇编"})
	makePub("s1", "slides", map[string]r.Value{"stage": {Option: &optMid}}, "teacher-a", "2025-spring", "public", nil)
	makePub("s2", "slides", map[string]r.Value{"stage": {Option: &optFinal}}, "teacher-a", "2025-spring", "public", nil)
	makePub("p1", "plain", map[string]r.Value{}, "teacher-a", "2025-spring", "public", nil)

	// Public & counts filtering by teacher_id, term_id, tag, access.
	page, err := h.s.List(h.ctx, reader, r.Filter{SchoolID: "school", CourseID: "course", TeacherID: "teacher-a", TermID: "2025-spring", Tag: "解析", Access: "public", Sort: "newest", Limit: 10}, false, false)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != r1.ID {
		t.Fatalf("filtered list: %+v %v", page, err)
	}
	restrictedCounts, err := h.s.Counts(h.ctx, reader, r.Filter{SchoolID: "school", CourseID: "course", TeacherID: "teacher-b", Access: "restricted", Tag: "真题"}, []string{"course"})
	if err != nil || restrictedCounts.Total != 1 || len(restrictedCounts.Courses) != 1 || restrictedCounts.Courses[0].Count != 1 {
		t.Fatalf("filtered counts: %+v %v", restrictedCounts, err)
	}
	if _, err := h.s.List(h.ctx, reader, r.Filter{SchoolID: "school", TeacherID: "unknown-teacher", Limit: 10}, false, false); !errors.Is(err, r.ErrInput) {
		t.Fatalf("unknown teacher not rejected: %v", err)
	}
	if _, err := h.s.Counts(h.ctx, reader, r.Filter{SchoolID: "school", TeacherID: "other-school-teacher"}, nil); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("cross-school teacher not denied: %v", err)
	}

	// Cursor scope binds new filters.
	f := r.Filter{SchoolID: "school", Tag: "真题", Sort: "newest", Limit: 1}
	firstPage, err := h.s.List(h.ctx, reader, f, false, false)
	if err != nil || firstPage.NextCursor == "" || firstPage.Items[0].ID != r2.ID {
		t.Fatalf("tag page 1: %+v %v", firstPage, err)
	}
	f.After = firstPage.NextCursor
	f.Access = "public"
	if _, err := h.s.List(h.ctx, reader, f, false, false); !errors.Is(err, r.ErrInput) {
		t.Fatalf("cursor reused across access filter: %v", err)
	}

	// Dynamic integer & choice group_by facets and ungrouped count.
	examCounts, err := h.s.Counts(h.ctx, reader, r.Filter{SchoolID: "school", CourseID: "course", CategoryID: "exam"}, []string{"course"})
	if err != nil || examCounts.Total != 3 || examCounts.GroupBy != "year" || examCounts.UngroupedCount != 1 || len(examCounts.Groups) != 2 || examCounts.Groups[0].Key != "2025" || examCounts.Groups[0].Count != 1 || examCounts.Groups[1].Key != "2024" || examCounts.Groups[1].Count != 1 {
		t.Fatalf("integer group facets: %+v %v", examCounts, err)
	}
	slideCounts, err := h.s.Counts(h.ctx, reader, r.Filter{SchoolID: "school", CategoryID: "slides"}, nil)
	if err != nil || slideCounts.GroupBy != "stage" || len(slideCounts.Groups) != 2 || slideCounts.Groups[0].Key != "final" || slideCounts.Groups[1].Key != "midterm" {
		t.Fatalf("choice group order should follow configured options: %+v %v", slideCounts, err)
	}
	plainCounts, err := h.s.Counts(h.ctx, reader, r.Filter{SchoolID: "school", CategoryID: "plain"}, nil)
	if err != nil || plainCounts.GroupBy != "" || len(plainCounts.Groups) != 0 || plainCounts.UngroupedCount != 0 {
		t.Fatalf("non-grouped category returned group facets: %+v %v", plainCounts, err)
	}

	// HTTP + generated SDK for /me filtering and /counts schema conformance.
	handler, err := httpapi.New(h.s, authenticator{})
	must(t, err)
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := sdk.NewClientWithResponses(server.URL)
	must(t, err)
	accessFilter := sdk.ListOwnResourcesParamsAccessRestricted
	tagFilter := "真题"
	ownResp, err := client.ListOwnResourcesWithResponse(h.ctx, &sdk.ListOwnResourcesParams{Access: &accessFilter, Tag: &tagFilter}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer owner")
		return nil
	})
	must(t, err)
	if ownResp.StatusCode() != 200 || ownResp.JSON200 == nil || ownResp.JSON200.Total != 1 || len(ownResp.JSON200.Items) != 1 || ownResp.JSON200.Items[0].Id != r2.ID {
		t.Fatalf("SDK ListOwnResources filter: status=%d body=%s", ownResp.StatusCode(), ownResp.Body)
	}

	doc, err := openapi3.NewLoader().LoadFromData(contracts.Document())
	must(t, err)
	raw, err := json.Marshal(examCounts)
	must(t, err)
	var decoded any
	must(t, json.Unmarshal(raw, &decoded))
	if err := doc.Components.Schemas["ResourceCounts"].Value.VisitJSON(decoded); err != nil {
		t.Fatalf("ResourceCounts violates OpenAPI schema: %v", err)
	}
}
