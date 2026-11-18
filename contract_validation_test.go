package resources_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/contracts"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"github.com/StuHelper/stuhelper-resources/sdk"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestContractAndInputBoundaries(t *testing.T) {
	h := uiResourceFixture(t)
	cfg := h.cfg
	cfg.MaxPackageFiles = 100
	cfg.MaxPackageParts = 100
	var e error
	h.s, e = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: countCatalog{h.catalog}, Files: h.files, Moderation: h.moderation}, cfg)
	must(t, e)

	first := h.publish(t, h.draft(t, "cv-1", "public"))
	h.clock.Add(time.Hour)
	second := h.publish(t, h.draft(t, "cv-2", "public"))

	handler, e := httpapi.New(h.s, authenticator{})
	must(t, e)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	doc, e := openapi3.NewLoader().LoadFromData(contracts.Document())
	must(t, e)
	router, e := legacy.NewRouter(doc)
	must(t, e)

	validateResp := func(req *http.Request, resp *http.Response, body []byte) {
		t.Helper()
		route, params, err := router.FindRoute(req)
		must(t, err)
		reqIn := &openapi3filter.RequestValidationInput{
			Request:    req,
			PathParams: params,
			Route:      route,
			Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		err = openapi3filter.ValidateResponse(h.ctx, &openapi3filter.ResponseValidationInput{
			RequestValidationInput: reqIn,
			Status:                 resp.StatusCode,
			Header:                 resp.Header,
			Body:                   io.NopCloser(bytes.NewReader(body)),
			Options:                &openapi3filter.Options{IncludeResponseStatus: true},
		})
		if err != nil {
			t.Fatalf("OpenAPI response validation failed for %s %s: %v", req.Method, req.URL.Path, err)
		}
	}

	do := func(method, path, body, token string) (*http.Request, *http.Response, []byte) {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, srv.URL+path, rdr)
		must(t, err)
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Idempotency-Key", "cv-key-"+fmt.Sprint(time.Now().UnixNano()))
		resp, err := srv.Client().Do(req)
		must(t, err)
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		must(t, err)
		return req, resp, raw
	}

	// A-06: sort=newest next_cursor must satisfy OpenAPI ResourcePage schema.
	req, resp, raw := do("GET", "/api/v1/resources?school_id=school&sort=newest&limit=1", "", "Bearer reader")
	if resp.StatusCode != 200 {
		t.Fatalf("list newest failed: %d %s", resp.StatusCode, string(raw))
	}
	validateResp(req, resp, raw)

	// A-13.1: Generated Go SDK passes full filter set on ListOwnResources (/api/v1/resources/me).
	client, e := sdk.NewClientWithResponses(srv.URL)
	must(t, e)
	cat := "exam"
	school := "school"
	q := "离散"
	ownResp, e := client.ListOwnResourcesWithResponse(h.ctx, &sdk.ListOwnResourcesParams{SchoolId: &school, CategoryId: &cat, Query: &q}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer owner")
		return nil
	})
	must(t, e)
	if ownResp.StatusCode() != 200 || ownResp.JSON200 == nil || len(ownResp.JSON200.Items) != 2 {
		t.Fatalf("SDK ListOwnResources filter failed: status=%d body=%s", ownResp.StatusCode(), string(ownResp.Body))
	}

	// A-13.3: Preview (application/pdf) and PackagePart (application/zip) must pass OpenAPI response validation.
	pkg, e := h.s.CreatePackage(h.ctx, reader, "cv-pkg", r.PackageRequest{ResourceIDs: []string{first.ID, second.ID}})
	must(t, e)
	req, resp, raw = do("GET", "/api/v1/resources/items/"+first.ID+"/preview", "", "Bearer reader")
	if resp.StatusCode != 200 {
		t.Fatalf("preview failed: %d", resp.StatusCode)
	}
	validateResp(req, resp, raw)

	req, resp, raw = do("GET", "/api/v1/resources/packages/"+pkg.ID+"/parts/1", "", "Bearer reader")
	if resp.StatusCode != 200 {
		t.Fatalf("package download failed: %d", resp.StatusCode)
	}
	validateResp(req, resp, raw)

	// Empty file_type= on /counts must be rejected with 400 just like /resources.
	_, resp, _ = do("GET", "/api/v1/resources/counts?school_id=school&file_type=", "", "Bearer reader")
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for empty file_type on /counts, got %d", resp.StatusCode)
	}

	// A-13.2: PackageRequest with > 100 resource_ids (101 items) must be accepted by OpenAPI when within MaxPackageFiles*MaxPackageParts.
	manyIDs := []string{first.ID, second.ID}
	for i := 0; i < 99; i++ {
		manyIDs = append(manyIDs, fmt.Sprintf("missing-%03d", i))
	}
	pkgBody, e := json.Marshal(r.PackageRequest{ResourceIDs: manyIDs})
	must(t, e)
	req, resp, raw = do("POST", "/api/v1/resources/packages", string(pkgBody), "Bearer reader")
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for 101-item package request, got %d %s", resp.StatusCode, string(raw))
	}
	validateResp(req, resp, raw)

	t.Run("maximum explicit package reaches service authorization", func(t *testing.T) {
		ids := make([]string, 10000)
		for i := range ids {
			ids[i] = fmt.Sprintf("%0128d", i)
		}
		payload, err := json.Marshal(r.PackageRequest{ResourceIDs: ids})
		must(t, err)
		h.auth.set("package", true)
		defer h.auth.set("package", false)
		_, resp, _ := do("POST", "/api/v1/resources/packages", string(payload), "Bearer reader")
		if resp.StatusCode != 403 {
			t.Fatalf("expected package permission check, got %d", resp.StatusCode)
		}
	})
	t.Run("multibyte attribute filter", func(t *testing.T) {
		_, err := h.s.PutCategory(h.ctx, global, "notes", "cv-notes", r.CategoryWrite{Enabled: true, Names: r.Names{"zh-CN": "笔记"}, Fields: []r.Field{{Key: "label", Names: r.Names{"zh-CN": "标签"}, Kind: "text", Filterable: true, MaxLength: 1000}}})
		must(t, err)
		value := strings.Repeat("课", 700)
		attrs, err := json.Marshal(map[string]r.Value{"label": {Text: &value}})
		must(t, err)
		_, resp, _ := do("GET", "/api/v1/resources?school_id=school&category_id=notes&attributes="+url.QueryEscape(string(attrs)), "", "Bearer reader")
		if resp.StatusCode != 200 {
			t.Fatalf("expected valid Unicode filter, got %d", resp.StatusCode)
		}
	})

	// A-13.4: Boundary-length valid Principal.Key() (1026 chars) accepted in GrantWrite, while non-canonical subject_key & control/invalid-UTF8 Principal rejected.
	maxPrincipal := r.Principal{Issuer: "https://" + strings.Repeat("a", 504), Subject: strings.Repeat("b", 256)}
	if !maxPrincipal.Valid() || len(maxPrincipal.Key()) != 1026 {
		t.Fatalf("unexpected max principal validity=%v len=%d", maxPrincipal.Valid(), len(maxPrincipal.Key()))
	}
	grantBody, e := json.Marshal(r.GrantWrite{Revision: 0, SubjectKey: maxPrincipal.Key(), Revoked: false})
	must(t, e)
	req, resp, raw = do("PUT", "/api/v1/resources/items/"+first.ID+"/grants/max-grant", string(grantBody), "Bearer global")
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for 1026-char canonical subject_key, got %d %s", resp.StatusCode, string(raw))
	}
	validateResp(req, resp, raw)

	if _, e = h.s.PutGrant(h.ctx, manager, first.ID, "bad-grant", "bad-grant-k", r.GrantWrite{SubjectKey: "raw-username"}); !errors.Is(e, r.ErrInput) {
		t.Fatalf("expected ErrInput for non-canonical subject_key, got %v", e)
	}
	for _, bad := range []r.Principal{
		{Issuer: "bad\nissuer", Subject: "sub"},
		{Issuer: "issuer", Subject: "bad\x00sub"},
		{Issuer: string([]byte{0xff}), Subject: "sub"},
	} {
		if bad.Valid() {
			t.Fatalf("invalid principal accepted: %+v", bad)
		}
	}

	// A-13.5: Unknown nested fields or null in attributes query parameter must be rejected with 400.
	for _, badAttr := range []string{`{"year":{"number":2025,"extra":1}}`, `null`} {
		_, resp, _ = do("GET", "/api/v1/resources?school_id=school&category_id=exam&attributes="+url.QueryEscape(badAttr), "", "Bearer reader")
		if resp.StatusCode != 400 {
			t.Fatalf("expected 400 for attributes=%s, got %d", badAttr, resp.StatusCode)
		}
	}
}
