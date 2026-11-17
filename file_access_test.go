package resources_test

import (
	"context"
	"encoding/json"
	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type detailedAuthority struct {
	base    *authority
	failure error
}

func (a *detailedAuthority) Check(ctx context.Context, p r.Principal, action, scope string) error {
	if strings.HasPrefix(action, "download.") && a.failure != nil {
		return a.failure
	}
	return a.base.Check(ctx, p, action, scope)
}
func TestPreviewAndDownloadShareCurrentRightsAndExactMissingConditions(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "files", "restricted"))
	policy := &detailedAuthority{base: h.auth}
	var e error
	h.s, e = r.New(h.repo, r.Dependencies{Authority: policy, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}, h.cfg)
	must(t, e)
	handler, e := httpapi.New(h.s, authenticator{})
	must(t, e)
	call := func(action, token string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/v1/resources/items/"+item.ID+"/"+action, nil)
		req.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s %d %s", action, w.Code, w.Body.String())
		}
		return w
	}
	check := func(wantStatus int, missing, unavailable []string) {
		t.Helper()
		before := h.files.downloads
		for _, action := range []string{"download", "preview"} {
			w := call(action, "Bearer reader", wantStatus)
			var result struct {
				Missing     []string `json:"missing_conditions"`
				Unavailable []string `json:"unavailable_conditions"`
			}
			must(t, json.Unmarshal(w.Body.Bytes(), &result))
			if !slices.Equal(missing, result.Missing) || !slices.Equal(unavailable, result.Unavailable) {
				t.Fatal("wrong missing condition", w.Body.String())
			}
		}
		if h.files.downloads != before {
			t.Fatal("denied request read file bytes")
		}
	}
	call("preview", "", 401)
	call("download", "", 401)
	check(403, []string{"resource_entitlement"}, nil)
	if !h.s.Capabilities().PurchaseAvailable {
		t.Fatal("purchase support missing")
	}
	policy.failure = &r.AccessFailure{Reason: "requirements_missing", Missing: []string{"identity_verified", "phone_bound"}, RuleVersion: "custom-2"}
	check(403, []string{"identity_verified", "phone_bound"}, nil)
	policy.failure = &r.AccessFailure{Reason: "service_unavailable", Missing: []string{"phone_bound"}, Unavailable: []string{"identity_verified"}, RuleVersion: "custom-2"}
	check(503, []string{"phone_bound"}, []string{"identity_verified"})
	owned, e := h.s.Get(h.ctx, uploader, item.ID)
	must(t, e)
	if owned.Current == nil || owned.DownloadAllowed || owned.AccessReason != "service_unavailable" || !slices.Equal(owned.UnavailableConditions, []string{"identity_verified"}) {
		t.Fatal("qualification outage hid owner record", owned)
	}

	policy.failure = &r.AccessFailure{Reason: "requirements_missing", Missing: []string{"forged_condition"}}
	check(503, nil, nil)
	policy.failure = nil

	grant, e := h.s.PutGrant(h.ctx, manager, item.ID, "reader-grant", "grant", r.GrantWrite{SubjectKey: reader.Key()})
	must(t, e)
	download := call("download", "Bearer reader", 200)
	preview := call("preview", "Bearer reader", 200)
	if preview.Body.String() != download.Body.String() || !strings.HasPrefix(preview.Header().Get("Content-Disposition"), "inline") || preview.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("different preview or missing sandbox")
	}
	_, e = h.s.PutGrant(h.ctx, manager, item.ID, grant.ID, "revoke", r.GrantWrite{Revision: grant.Revision, SubjectKey: reader.Key(), Revoked: true})
	must(t, e)
	check(403, []string{"resource_entitlement"}, nil)
	policy.failure = &r.AccessFailure{Reason: "requirements_missing", Missing: []string{"campus_eligibility"}, RuleVersion: "default"}
	// Scope-authorized administrators retain the same exception for both routes.
	call("preview", "Bearer global", 200)
	call("download", "Bearer global", 200)
}

type directRangeFiles struct {
	*files
	rangeCalls int
}

func (d *directRangeFiles) DownloadRange(_ context.Context, _ r.Principal, id string, br r.ByteRange, w io.Writer) error {
	d.mu.Lock()
	d.rangeCalls++
	d.mu.Unlock()
	full := d.payload(id)
	_, err := io.WriteString(w, full[br.Offset:br.Offset+br.Length])
	return err
}

func TestPreviewAndDownloadETagConditionalAndByteRangeDelivery(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "range-etag", "restricted"))
	grant, err := h.s.PutGrant(h.ctx, manager, item.ID, "range-grant", "grant-range", r.GrantWrite{SubjectKey: reader.Key()})
	must(t, err)

	handler, err := httpapi.New(h.s, authenticator{})
	must(t, err)
	server := httptest.NewServer(handler)
	defer server.Close()

	doReq := func(action, token string, headers map[string]string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest("GET", server.URL+"/api/v1/resources/items/"+item.ID+"/"+action, nil)
		must(t, err)
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := server.Client().Do(req)
		must(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		must(t, err)
		return resp, string(body)
	}

	wantETag := `"` + item.Current.File.SHA256 + `"`
	resp200, fullBody := doReq("download", "Bearer reader", nil)
	if resp200.StatusCode != 200 || len(fullBody) != 60 || resp200.Header.Get("ETag") != wantETag || resp200.Header.Get("Content-Length") != "60" || resp200.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("200 headers/body mismatch: status=%d len=%d headers=%v", resp200.StatusCode, len(fullBody), resp200.Header)
	}

	// Conditional 304 Not Modified without reading file bytes.
	downloadsBefore := h.files.downloads
	for _, inm := range []string{wantETag, "W/" + wantETag, `"other", ` + wantETag} {
		resp304, body304 := doReq("preview", "Bearer reader", map[string]string{"If-None-Match": inm})
		if resp304.StatusCode != 304 || body304 != "" || resp304.Header.Get("ETag") != wantETag || resp304.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("304 mismatch for %s: status=%d body=%q headers=%v", inm, resp304.StatusCode, body304, resp304.Header)
		}
	}
	if h.files.downloads != downloadsBefore {
		t.Fatal("304 response read file bytes")
	}

	// Fallback byte-range slicing on legacy Files port.
	respRange, bodyRange := doReq("preview", "Bearer reader", map[string]string{"Range": "bytes=0-9"})
	if respRange.StatusCode != 206 || bodyRange != fullBody[:10] || respRange.Header.Get("Content-Range") != "bytes 0-9/60" || respRange.Header.Get("Content-Length") != "10" {
		t.Fatalf("fallback 206 mismatch: status=%d range=%q body=%q", respRange.StatusCode, respRange.Header.Get("Content-Range"), bodyRange)
	}

	// Direct RangeFiles port implementation.
	rf := &directRangeFiles{files: h.files}
	svcRange, err := r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: rf, Moderation: h.moderation}, h.cfg)
	must(t, err)
	handlerRange, err := httpapi.New(svcRange, authenticator{})
	must(t, err)
	serverRange := httptest.NewServer(handlerRange)
	defer serverRange.Close()

	doDirect := func(headers map[string]string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest("GET", serverRange.URL+"/api/v1/resources/items/"+item.ID+"/download", nil)
		must(t, err)
		req.Header.Set("Authorization", "Bearer reader")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := serverRange.Client().Do(req)
		must(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		must(t, err)
		return resp, string(body)
	}

	downloadsBefore = h.files.downloads
	for _, tc := range []struct {
		rng, wantCR, wantBody string
	}{
		{"bytes=5-14", "bytes 5-14/60", fullBody[5:15]},
		{"bytes=50-", "bytes 50-59/60", fullBody[50:]},
		{"bytes=-8", "bytes 52-59/60", fullBody[52:]},
		{"bytes=55-999", "bytes 55-59/60", fullBody[55:]},
	} {
		resp, body := doDirect(map[string]string{"Range": tc.rng, "If-Range": wantETag})
		if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != tc.wantCR || resp.Header.Get("Content-Length") != strconv.Itoa(len(tc.wantBody)) || body != tc.wantBody {
			t.Fatalf("direct range %s: status=%d cr=%q body=%q", tc.rng, resp.StatusCode, resp.Header.Get("Content-Range"), body)
		}
	}
	if rf.rangeCalls != 4 || h.files.downloads != downloadsBefore {
		t.Fatalf("expected 4 direct range calls and 0 full downloads, got range=%d full=%d", rf.rangeCalls, h.files.downloads-downloadsBefore)
	}

	// Mismatched If-Range falls back to 200 full body.
	respIfRange, bodyIfRange := doDirect(map[string]string{"Range": "bytes=0-9", "If-Range": `"stale-etag"`})
	if respIfRange.StatusCode != 200 || bodyIfRange != fullBody {
		t.Fatalf("mismatched If-Range expected 200 full body, got %d len=%d", respIfRange.StatusCode, len(bodyIfRange))
	}

	// Unsatisfiable / invalid Range returns 416 with Content-Range: bytes */60.
	for _, bad := range []string{"bytes=60-70", "bytes=20-10", "bytes=-0", "bytes=invalid"} {
		resp416, _ := doDirect(map[string]string{"Range": bad})
		if resp416.StatusCode != 416 || resp416.Header.Get("Content-Range") != "bytes */60" {
			t.Fatalf("bad range %s: status=%d cr=%q", bad, resp416.StatusCode, resp416.Header.Get("Content-Range"))
		}
	}

	// Revoking entitlement immediately denies both If-None-Match and Range (never returns 304 or 206).
	_, err = h.s.PutGrant(h.ctx, manager, item.ID, grant.ID, "revoke-range", r.GrantWrite{Revision: grant.Revision, SubjectKey: reader.Key(), Revoked: true})
	must(t, err)
	respDenied304, _ := doDirect(map[string]string{"If-None-Match": wantETag})
	respDenied206, _ := doDirect(map[string]string{"Range": "bytes=0-9"})
	if respDenied304.StatusCode != 403 || respDenied206.StatusCode != 403 {
		t.Fatalf("revoked user got %d / %d on conditional/range request", respDenied304.StatusCode, respDenied206.StatusCode)
	}
}
