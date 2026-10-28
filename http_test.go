package resources_test

import (
	"bytes"
	"context"
	"encoding/json"
	resources "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"github.com/StuHelper/stuhelper-resources/sdk"
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
func TestHTTPContractSDKAndUnknownInput(t *testing.T) {
	h := newFixture(t)
	handler, e := httpapi.New(h.s, authenticator{})
	must(t, e)
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
	forged := map[string]any{"revision": 0, "metadata": metadata("public")}
	encoded, e := json.Marshal(forged)
	must(t, e)
	encoded = bytes.Replace(encoded, []byte(`"year":{"number":2025}`), []byte(`"year":{"number":2025,"hidden":"forged"}`), 1)
	response = send("POST", "/api/v1/resources", string(encoded), "Bearer owner")
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("nested unknown field accepted", response.StatusCode)
	}
}
