package resources_test

import (
	"context"
	"encoding/json"
	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"net/http/httptest"
	"slices"
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
