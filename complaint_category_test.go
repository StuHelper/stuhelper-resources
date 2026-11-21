package resources_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"github.com/StuHelper/stuhelper-resources/postgres"
)

func TestResourceComplaintCategoryHTTPAndDelivery(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "categorized", "public"))
	h.auth.set("discover", true)
	handler, err := httpapi.New(h.s, authenticator{})
	must(t, err)
	handler = contractValidatingHandler(t, handler)
	good := r.Request{Kind: "complaint", Category: "other", Reason: strings.Repeat("学", 2000), Contact: "contact", Evidence: []string{"evidence"}}
	post := func(t *testing.T, key string, in r.Request, status int) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(in)
		must(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/resources/items/"+item.ID+"/requests", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer reader")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("status: got %d want %d, response %s", rec.Code, status, rec.Body.String())
		}
		return rec
	}
	for _, test := range []struct {
		name     string
		category string
		reason   string
	}{
		{"missing-category", "", good.Reason},
		{"unknown-category", "unknown_category", good.Reason},
		{"blank-reason", "other", " \t\u3000"},
		{"oversize-reason", "other", good.Reason + "学"},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := good
			in.Category, in.Reason = test.category, test.reason
			rec := post(t, test.name, in, http.StatusBadRequest)
			var problem struct{ Code string }
			must(t, json.Unmarshal(rec.Body.Bytes(), &problem))
			if problem.Code != "resources_invalid_input" {
				t.Fatalf("unexpected error code %q", problem.Code)
			}
		})
	}
	var count int
	must(t, h.admin.QueryRowContext(h.ctx, `SELECT count(*) FROM resources.requests`).Scan(&count))
	if count != 0 {
		t.Fatalf("invalid complaints created %d records", count)
	}

	var receipt r.RequestReceipt
	must(t, json.Unmarshal(post(t, "categorized", good, http.StatusAccepted).Body.Bytes(), &receipt))
	if receipt.ID == "" || receipt.Category != "other" || receipt.ResourceTitle != "" {
		t.Fatalf("unexpected receipt %+v", receipt)
	}
	replayed := post(t, "categorized", good, http.StatusAccepted)
	var again r.RequestReceipt
	must(t, json.Unmarshal(replayed.Body.Bytes(), &again))
	if again != receipt {
		t.Fatalf("retry returned %+v, want %+v", again, receipt)
	}
	changed := good
	changed.Category = "malware"
	post(t, "categorized", changed, http.StatusConflict)
	h.drain(t)
	if h.moderation.requests != 1 || !reflect.DeepEqual(h.moderation.lastRequest, good) {
		t.Fatal("expected one delivery with the original category and complaint materials")
	}
	status, err := h.s.RequestStatus(h.ctx, reader, receipt.ID)
	must(t, err)
	if status.Category != "other" || status.TaskID == "" {
		t.Fatalf("missing category or task ID in receipt detail: %+v", status)
	}
	page, err := h.s.Requests(h.ctx, reader, 10, "")
	must(t, err)
	if len(page.Items) != 1 || page.Items[0].Category != "other" {
		t.Fatalf("unexpected receipt list %+v", page.Items)
	}
}

func TestLegacyComplaintCategoryMigrationPreservesReplay(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "legacy-category", "public"))
	valid := r.Request{Kind: "complaint", Category: "copyright", Reason: "Identify reused material", Contact: "contact", Evidence: []string{"evidence"}}
	receipt, err := h.s.CreateRequest(h.ctx, reader, item.ID, "valid-category", valid)
	must(t, err)
	h.drain(t)

	old, err := h.s.CreateRequest(h.ctx, reader, item.ID, "old-category", valid)
	must(t, err)
	// Seed the pre-classification row, queued job and exact original replay digest.
	original := valid
	original.Category = ""
	raw, err := json.Marshal(original)
	must(t, err)
	digest := sha256.Sum256(raw)
	_, err = h.admin.ExecContext(h.ctx, `UPDATE resources.requests SET data=data #- '{Request,category}' #- '{Receipt,category}' WHERE id=$1`, old.ID)
	must(t, err)
	_, err = h.admin.ExecContext(h.ctx, `UPDATE resources.replays SET data=jsonb_set(data #- '{Result,category}','{Digest}',to_jsonb($2::text)) WHERE data#>>'{Result,id}'=$1`,
		old.ID, hex.EncodeToString(digest[:]))
	must(t, err)
	for i := 0; i < 2; i++ {
		must(t, postgres.Migrate(h.ctx, h.admin))
	}
	replay, err := h.s.CreateRequest(h.ctx, reader, item.ID, "old-category", original)
	must(t, err)
	if replay.ID != old.ID || replay.Category != "other" {
		t.Fatalf("legacy replay lost category or changed receipt: %+v", replay)
	}
	if _, err = h.s.CreateRequest(h.ctx, reader, item.ID, "new-unclassified", original); !errors.Is(err, r.ErrInput) {
		t.Fatalf("new request without category was accepted: %v", err)
	}
	preserved, err := h.s.RequestStatus(h.ctx, reader, receipt.ID)
	must(t, err)
	if preserved.Category != "copyright" {
		t.Fatalf("migration replaced a valid category: %+v", preserved)
	}
	h.drain(t)
	original.Category = "other"
	if h.moderation.requests != 2 || !reflect.DeepEqual(h.moderation.lastRequest, original) {
		t.Fatal("migration changed original complaint materials or duplicated delivery")
	}
	migrated, err := h.s.RequestStatus(h.ctx, reader, old.ID)
	must(t, err)
	if migrated.Category != "other" || migrated.TaskID == "" {
		t.Fatalf("missing migrated category or task ID: %+v", migrated)
	}
}
