package resources_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type countingRepository struct {
	r.Repository
	txs atomic.Int64
}

func (c *countingRepository) Transaction(ctx context.Context, run func(r.Transaction) error) error {
	c.txs.Add(1)
	return c.Repository.Transaction(ctx, run)
}

func TestBatchQueriesRequestAuthReuseAndVersionIndexes(t *testing.T) {
	h := newFixture(t)
	countedRepo := &countingRepository{Repository: h.repo}
	cfg := h.cfg
	cfg.MaxPackageFiles = 6
	cfg.MaxPackageBytes = 1000
	var e error
	h.s, e = r.New(countedRepo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}, cfg)
	must(t, e)

	ids := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		access := "public"
		if i%3 == 0 {
			access = "restricted"
		}
		item := h.publish(t, h.draft(t, fmt.Sprintf("perf-%d", i), access))
		if access == "restricted" {
			h.purchase(t, item.ID, fmt.Sprintf("perf-ord-%d", i), 1, false)
		}
		ids = append(ids, item.ID)
	}

	reset := func() {
		countedRepo.txs.Store(0)
		h.auth.mu.Lock()
		h.auth.calls = map[string]int{}
		h.auth.mu.Unlock()
	}

	// A-08.1: CreatePackage reuses request-scoped Authority checks, batches DB reads, and avoids redundant GetPackage on fresh creation.
	reset()
	pkg, e := h.s.CreatePackage(h.ctx, reader, "perf-pkg", r.PackageRequest{ResourceIDs: ids})
	must(t, e)
	if len(pkg.Parts) != 2 {
		t.Fatalf("expected 2 package parts, got %d", len(pkg.Parts))
	}
	h.auth.mu.Lock()
	createDiscoverCalls := h.auth.calls["discover:school"]
	createPubCalls := h.auth.calls["download.public:school"]
	createResCalls := h.auth.calls["download.restricted:school"]
	h.auth.mu.Unlock()
	if createDiscoverCalls != 1 || createPubCalls != 1 || createResCalls != 1 {
		t.Fatalf("CreatePackage Authority RPC storm: discover=%d public=%d restricted=%d", createDiscoverCalls, createPubCalls, createResCalls)
	}
	if txCount := countedRepo.txs.Load(); txCount > 4 {
		t.Fatalf("CreatePackage opened %d DB transactions for 12 items (expected <= 4)", txCount)
	}

	// Idempotent replay of CreatePackage still rechecks current rights cleanly.
	replayed, e := h.s.CreatePackage(h.ctx, reader, "perf-pkg", r.PackageRequest{ResourceIDs: ids})
	must(t, e)
	if replayed.ID != pkg.ID {
		t.Fatalf("replay returned different package ID: %s vs %s", replayed.ID, pkg.ID)
	}

	// GetPackage and DownloadPackage also batch DB reads and cache request Authority checks.
	reset()
	_, e = h.s.GetPackage(h.ctx, reader, pkg.ID)
	must(t, e)
	h.auth.mu.Lock()
	getPkgDiscover := h.auth.calls["discover:school"]
	h.auth.mu.Unlock()
	if getPkgDiscover != 1 || countedRepo.txs.Load() > 3 {
		t.Fatalf("GetPackage inefficient: discover=%d txs=%d", getPkgDiscover, countedRepo.txs.Load())
	}

	reset()
	var partBuf bytes.Buffer
	must(t, h.s.DownloadPackage(h.ctx, reader, pkg.ID, 1, &partBuf))
	h.auth.mu.Lock()
	dlPkgDiscover := h.auth.calls["discover:school"]
	h.auth.mu.Unlock()
	if dlPkgDiscover != 1 || countedRepo.txs.Load() > 3 {
		t.Fatalf("DownloadPackage inefficient: discover=%d txs=%d", dlPkgDiscover, countedRepo.txs.Load())
	}

	// A-08.2: List batches version and entitlement reads into <= 2 DB transactions instead of 4*N.
	reset()
	page, e := h.s.List(h.ctx, reader, r.Filter{SchoolID: "school", Sort: "newest", Limit: 20}, false, false)
	must(t, e)
	if len(page.Items) != 12 || page.Total != 12 {
		t.Fatalf("unexpected page: len=%d total=%d", len(page.Items), page.Total)
	}
	if txCount := countedRepo.txs.Load(); txCount > 3 {
		t.Fatalf("List opened %d DB transactions for 12 items (expected <= 3)", txCount)
	}

	// HTTP /download and /preview check permissions and load DB metadata once per request, not twice.
	handler, e := httpapi.New(h.s, authenticator{})
	must(t, e)
	for _, action := range []string{"download", "preview"} {
		reset()
		req := httptest.NewRequest("GET", "/api/v1/resources/items/"+ids[1]+"/"+action, nil)
		req.Header.Set("Authorization", "Bearer reader")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("HTTP %s status=%d body=%s", action, w.Code, w.Body.String())
		}
		h.auth.mu.Lock()
		disc := h.auth.calls["discover:school"]
		dl := h.auth.calls["download.public:school"]
		h.auth.mu.Unlock()
		if disc != 1 || dl != 1 || countedRepo.txs.Load() > 2 {
			t.Fatalf("HTTP %s duplicated checks: discover=%d download=%d txs=%d", action, disc, dl, countedRepo.txs.Load())
		}
	}

	t.Run("download denial reason", func(t *testing.T) {
		h.auth.set("download.public", true)
		defer h.auth.set("download.public", false)
		req := httptest.NewRequest("GET", "/api/v1/resources/items/"+ids[1]+"/download", nil)
		req.Header.Set("Authorization", "Bearer reader")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var problem struct {
			Reason string `json:"access_reason"`
		}
		must(t, json.Unmarshal(w.Body.Bytes(), &problem))
		if w.Code != 403 || problem.Reason != "permission_denied" {
			t.Fatalf("status=%d reason=%s", w.Code, problem.Reason)
		}
	})
	t.Run("visibility before entitlement facts", func(t *testing.T) {
		h.auth.set("discover", true)
		defer h.auth.set("discover", false)
		var role string
		must(t, h.db.QueryRow("SELECT current_user").Scan(&role))
		_, err := h.admin.Exec("REVOKE SELECT ON resources.purchases FROM " + role)
		must(t, err)
		defer func() { _, err := h.admin.Exec("GRANT SELECT ON resources.purchases TO " + role); must(t, err) }()
		_, err = h.s.Get(h.ctx, reader, ids[0])
		if !errors.Is(err, r.ErrDenied) {
			t.Fatalf("denied details depended on entitlement facts: %v", err)
		}
	})
	t.Run("attributes index serves browse predicate", func(t *testing.T) {
		tx, err := h.db.Begin()
		must(t, err)
		defer tx.Rollback()
		_, err = tx.Exec("SET LOCAL enable_seqscan = off")
		must(t, err)
		var plan string
		must(t, tx.QueryRow(`EXPLAIN (FORMAT JSON) SELECT id FROM resources.versions v WHERE COALESCE(v.data->'Version'->'metadata'->'attributes','{}'::jsonb) @> '{"year":{"number":2025}}'::jsonb`).Scan(&plan))
		if !strings.Contains(plan, `"Index Name": "resources_versions_attributes"`) {
			t.Fatalf("no attribute index in plan: %s", plan)
		}
	})

	// A-09: Verify version filter indexes exist in schema.
	for _, idx := range []string{
		"resources_versions_course",
		"resources_versions_category",
		"resources_versions_directory",
		"resources_versions_attributes",
	} {
		var exists bool
		must(t, h.admin.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='resources' AND tablename='versions' AND indexname=$1)`, idx).Scan(&exists))
		if !exists {
			t.Fatalf("missing index %s on resources.versions", idx)
		}
	}
}
