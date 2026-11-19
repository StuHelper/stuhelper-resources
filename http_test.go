package resources_test

import (
	"bytes"
	"context"
	"encoding/json"
	resources "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/contracts"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"github.com/StuHelper/stuhelper-resources/sdk"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type authenticator struct{}

func (authenticator) Authenticate(r *http.Request) (resources.Principal, error) {
	switch r.Header.Get("Authorization") {
	case "Bearer reader":
		return reader, nil
	case "Bearer owner":
		return uploader, nil
	case "Bearer global":
		return global, nil
	default:
		return resources.Principal{}, resources.ErrUnauthenticated
	}
}
func contractValidatingHandler(t *testing.T, inner http.Handler) http.Handler {
	t.Helper()
	doc, e := openapi3.NewLoader().LoadFromData(contracts.Document())
	must(t, e)
	router, e := legacy.NewRouter(doc)
	must(t, e)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody []byte
		if r.Body != nil {
			reqBody, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)

		routeReq := r.Clone(r.Context())
		routeReq.Body = io.NopCloser(bytes.NewReader(reqBody))
		if route, params, routeErr := router.FindRoute(routeReq); routeErr == nil {
			reqInput := &openapi3filter.RequestValidationInput{
				Request:    routeReq,
				PathParams: params,
				Route:      route,
				Options: &openapi3filter.Options{
					AuthenticationFunc:    openapi3filter.NoopAuthenticationFunc,
					SkipSettingDefaults:   true,
					IncludeResponseStatus: true,
				},
			}
			respBytes := rec.Body.Bytes()
			if valErr := openapi3filter.ValidateResponse(r.Context(), &openapi3filter.ResponseValidationInput{
				RequestValidationInput: reqInput,
				Status:                 rec.Code,
				Header:                 rec.Header(),
				Body:                   io.NopCloser(bytes.NewReader(respBytes)),
				Options:                reqInput.Options,
			}); valErr != nil {
				t.Errorf("%s %s (%d) violated OpenAPI response schema: %v\nbody: %s", r.Method, r.URL.String(), rec.Code, valErr, string(respBytes))
			}
		}

		for k, vals := range rec.Header() {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

func TestHTTPContractSDKAndUnknownInput(t *testing.T) {
	h := newFixture(t)
	rawHandler, e := httpapi.New(h.s, authenticator{})
	must(t, e)
	handler := contractValidatingHandler(t, rawHandler)
	server := httptest.NewServer(handler)
	defer server.Close()
	client, e := sdk.NewClientWithResponses(server.URL)
	must(t, e)
	r := h.publish(t, h.draft(t, "http", "public"))
	got, e := client.GetResourceWithResponse(h.ctx, r.ID, func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer reader")
		return nil
	})
	must(t, e)
	if got.StatusCode() != 200 || got.JSON200 == nil || got.JSON200.Current == nil || got.JSON200.Current.Id != r.Current.ID {
		t.Fatalf("generated SDK: status=%d body=%s", got.StatusCode(), got.Body)
	}
	send := func(method, path, body, token string) *http.Response {
		t.Helper()
		req, e := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		must(t, e)
		req.Header.Set("Authorization", token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "http-command")
		response, e := server.Client().Do(req)
		must(t, e)
		return response
	}
	for _, body := range []string{`{"revision":0,"names":{"zh-CN":"分类"},"order":0,"fields":[],"group_by":""}`, `{"revision":0,"names":{"zh-CN":"分类"},"enabled":true,"order":0,"fields":[],"group_by":"","owner":"forged"}`} {
		response := send("PUT", "/api/v1/resources/categories/new", body, "Bearer global")
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal("required/unknown field accepted", response.StatusCode)
		}
	}
	response := send("GET", "/api/v1/resources?school_id=school&limit=101", "", "Bearer reader")
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal(response.StatusCode)
	}
	response = send("GET", "/api/v1/resources/items/"+r.ID+"/download", "", "")
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal(response.StatusCode)
	}
	response = send("GET", "/api/v1/resources/items/"+r.ID+"/download", "", "Bearer reader")
	body, e := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, e)
	if response.StatusCode != 200 || len(body) == 0 || !strings.Contains(response.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("download failed", response.StatusCode)
	}
	pending := h.draft(t, "http-submit", "public")
	revisionBody, e := json.Marshal(resources.Command{Revision: pending.Revision})
	must(t, e)
	response = send("POST", "/api/v1/resources/items/"+pending.ID+"/submit", string(revisionBody), "Bearer owner")
	response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatal("submission status differs from contract", response.StatusCode)
	}
	forged := map[string]any{"revision": 0, "metadata": metadata("public")}
	encoded, e := json.Marshal(forged)
	must(t, e)
	doc, e := openapi3.NewLoader().LoadFromData(contracts.Document())
	must(t, e)
	router, e := legacy.NewRouter(doc)
	must(t, e)
	checked, e := http.NewRequest("POST", server.URL+"/api/v1/resources", bytes.NewReader(encoded))
	must(t, e)
	checked.Header.Set("Content-Type", "application/json")
	checked.Header.Set("Idempotency-Key", "http-command")
	route, params, e := router.FindRoute(checked)
	must(t, e)
	must(t, openapi3filter.ValidateRequest(h.ctx, &openapi3filter.RequestValidationInput{Request: checked, PathParams: params, Route: route, Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, SkipSettingDefaults: true}}))
	response = send("POST", "/api/v1/resources", string(encoded), "Bearer owner")
	successBody, e := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, e)
	if response.StatusCode != 200 {
		t.Fatal("creation status differs from contract", response.StatusCode, string(successBody))
	}
	encoded = bytes.Replace(encoded, []byte(`"year":{"number":2025}`), []byte(`"year":{"number":2025,"hidden":"forged"}`), 1)
	response = send("POST", "/api/v1/resources", string(encoded), "Bearer owner")
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("nested unknown field accepted", response.StatusCode)
	}
}

func TestReceiptHTTPStatesMatchGeneratedContract(t *testing.T) {
	h := newFixture(t)
	draft := h.draft(t, "receipt-contract", "public")
	receipt, e := h.s.CreateRequest(h.ctx, uploader, draft.ID, "receipt", resources.Request{Kind: "withdrawal", Reason: "correction"})
	must(t, e)
	h.drain(t)
	doc, e := openapi3.NewLoader().LoadFromData(contracts.Document())
	must(t, e)
	status, e := h.s.RequestStatus(h.ctx, uploader, receipt.ID)
	must(t, e)
	page, e := h.s.Requests(h.ctx, uploader, 25, "")
	must(t, e)
	for name, value := range map[string]any{"RequestReceipt": status, "RequestPage": page} {
		b, e := json.Marshal(value)
		must(t, e)
		var decoded any
		must(t, json.Unmarshal(b, &decoded))
		if e = doc.Components.Schemas[name].Value.VisitJSON(decoded); e != nil {
			t.Fatalf("%s response violates contract: %v", name, e)
		}
	}
}

func TestCoverageHTTPAndAccessMethods(t *testing.T) {
	h := newFixture(t)
	rawHandler, e := httpapi.New(h.s, authenticator{})
	must(t, e)
	handler := contractValidatingHandler(t, rawHandler)
	body := `{"plan_id":"plan","revision":0,"rules":[{"school_id":"school","category_id":"exam"}]}`
	call := func(method, token, body string, want int) {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1/resources/coverage/plan", strings.NewReader(body))
		req.Header.Set("Authorization", token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "coverage-http")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%d: %s", w.Code, w.Body.String())
		}
		if want == 200 {
			var c sdk.Coverage
			must(t, json.Unmarshal(w.Body.Bytes(), &c))
			if c.PlanId != "plan" || len(c.Rules) != 1 || c.Revision != 1 {
				t.Fatal(c)
			}
		}
	}
	call("PUT", "Bearer reader", body, 403)
	call("PUT", "Bearer global", body, 200)
	call("PUT", "Bearer global", body, 200)
	call("GET", "Bearer reader", "", 200)
	call("PUT", "Bearer global", `{"plan_id":"plan","revision":1,"rules":[],"unexpected":true}`, 400)
	item := h.publish(t, h.draft(t, "methods", "restricted"))
	got, e := h.s.Get(h.ctx, reader, item.ID)
	must(t, e)
	if got.UnlockReason != "eligible" || len(got.AvailableMethods) != 1 || got.AvailableMethods[0] != "purchase" {
		t.Fatal(got)
	}
	h.purchase(t, item.ID, "http-order", 1, false)
	got, e = h.s.Get(h.ctx, reader, item.ID)
	must(t, e)
	if !got.DownloadAllowed || len(got.AvailableMethods) != 1 || got.AvailableMethods[0] != "download" {
		t.Fatal(got)
	}
	// Exercise newest pagination, preview, package archive, capabilities and upload profile contract validation.
	h.publish(t, h.draft(t, "second-page-item", "public"))
	pkg, e := h.s.CreatePackage(h.ctx, reader, "pkg-contract", resources.PackageRequest{ResourceIDs: []string{item.ID}})
	must(t, e)
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/api/v1/resources?school_id=school&sort=newest&limit=1", "Bearer reader", 200},
		{"/api/v1/resources/items/" + item.ID + "/preview", "Bearer reader", 200},
		{"/api/v1/resources/packages/" + pkg.ID + "/parts/1", "Bearer reader", 200},
		{"/api/v1/resources/capabilities", "Bearer reader", 200},
		{"/api/v1/resources/me/summary", "Bearer owner", 200},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", tc.token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s: got %d, want %d (%s)", tc.path, w.Code, tc.want, w.Body.String())
		}
	}
	doc, e := openapi3.NewLoader().LoadFromData(contracts.Document())
	must(t, e)
	if err := doc.Components.Schemas["UploadProfile"].Value.VisitJSON(map[string]any{"school_id": "school", "access": "public", "purpose": "resources", "extra": true}); err == nil {
		t.Fatal("UploadProfile accepted unknown property")
	}
}
