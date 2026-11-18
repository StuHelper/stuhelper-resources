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
	"github.com/StuHelper/stuhelper-resources/contracts"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	rp "github.com/StuHelper/stuhelper-resources/postgres"
	"github.com/getkin/kin-openapi/openapi3"
)

func TestRequestIDCorrelationOperationalReadinessStatsAndBoundedPruning(t *testing.T) {
	h := newFixture(t)
	handler, err := httpapi.New(h.s, authenticator{})
	must(t, err)
	doc, err := openapi3.NewLoader().LoadFromData(contracts.Document())
	must(t, err)

	// 1. Request ID from context and X-Request-ID header, plus rejection of malformed values.
	reqHeader := httptest.NewRequest("GET", "/api/v1/resources/items/missing", nil)
	reqHeader.Header.Set("Authorization", "Bearer reader")
	reqHeader.Header.Set("X-Request-ID", "trace-req_2026.01:abc")
	wHeader := httptest.NewRecorder()
	handler.ServeHTTP(wHeader, reqHeader)
	if wHeader.Code != 404 || wHeader.Header().Get("X-Request-ID") != "trace-req_2026.01:abc" {
		t.Fatalf("expected 404 with X-Request-ID header, got code=%d hdr=%q", wHeader.Code, wHeader.Header().Get("X-Request-ID"))
	}
	var problemPayload map[string]any
	must(t, json.Unmarshal(wHeader.Body.Bytes(), &problemPayload))
	if problemPayload["request_id"] != "trace-req_2026.01:abc" {
		t.Fatalf("missing request_id in Problem body: %v", problemPayload)
	}
	if err := doc.Components.Schemas["Problem"].Value.VisitJSON(problemPayload); err != nil {
		t.Fatalf("Problem payload violated OpenAPI schema: %v", err)
	}

	// Context request ID takes precedence over untrusted header.
	reqCtx := httptest.NewRequest("GET", "/api/v1/resources/items/missing", nil)
	reqCtx = reqCtx.WithContext(httpapi.WithRequestID(reqCtx.Context(), "host-ctx-id-99"))
	reqCtx.Header.Set("X-Request-ID", "client-spoofed-id")
	wCtx := httptest.NewRecorder()
	handler.ServeHTTP(wCtx, reqCtx)
	must(t, json.Unmarshal(wCtx.Body.Bytes(), &problemPayload))
	if wCtx.Code != 401 || wCtx.Header().Get("X-Request-ID") != "host-ctx-id-99" || problemPayload["request_id"] != "host-ctx-id-99" {
		t.Fatalf("context request ID precedence failed: code=%d hdr=%q body=%v", wCtx.Code, wCtx.Header().Get("X-Request-ID"), problemPayload)
	}

	// Unsafe or overlong X-Request-ID is ignored rather than echoed.
	for _, bad := range []string{"bad space", "bad\r\ninject", strings.Repeat("x", 129)} {
		reqBad := httptest.NewRequest("GET", "/api/v1/resources/items/missing", nil)
		reqBad.Header.Set("X-Request-ID", bad)
		wBad := httptest.NewRecorder()
		handler.ServeHTTP(wBad, reqBad)
		var badPayload map[string]any
		must(t, json.Unmarshal(wBad.Body.Bytes(), &badPayload))
		if wBad.Header().Get("X-Request-ID") != "" || badPayload["request_id"] != nil {
			t.Fatalf("unsafe X-Request-ID echoed for %q: hdr=%q body=%v", bad, wBad.Header().Get("X-Request-ID"), badPayload)
		}
	}

	// 2. Readiness probe & OperationalStats.
	must(t, h.s.Ready(h.ctx))
	if err := (&rp.Repository{DB: h.admin}).Ready(h.ctx); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("admin role accepted by Ready: %v", err)
	}

	pub := h.publish(t, h.draft(t, "ops-pub", "public"))
	pendingDraft := h.draft(t, "ops-pending", "public")
	_, err = h.s.Submit(h.ctx, uploader, pendingDraft.ID, "ops-submit", r.Command{Revision: pendingDraft.Revision})
	must(t, err)
	h.clock.Add(5 * time.Minute)

	stats, err := h.s.OperationalStats(h.ctx)
	must(t, err)
	if stats.Records != 2 || stats.MaxRecords != h.cfg.MaxRecords || stats.RecordUtilization <= 0 || stats.PendingJobs != 1 || stats.DoneJobs < 2 || stats.ActiveJobs != 1 || stats.PendingOutbox == 0 || stats.OldestPendingOutboxAt == nil || stats.OldestPendingOutboxAge < 5*time.Minute {
		t.Fatalf("unexpected OperationalStats: %+v", stats)
	}

	// 3. Bounded batch pruning under restricted runtime DB role.
	if _, err := h.repo.PruneOutbox(h.ctx, time.Time{}, 10); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected ErrInput on zero cutoff: %v", err)
	}
	if _, err := h.repo.PruneJobs(h.ctx, h.clock.Now(), 1001); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected ErrInput on batch > 1000: %v", err)
	}
	if _, err := h.repo.PruneSubmissionLimits(h.ctx, h.clock.Now(), 0); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected ErrInput on batch 0: %v", err)
	}

	// Confirm two outbox events, set one sent_at in the past, verify PruneOutbox deletes only the old confirmed event and keeps unconfirmed ones.
	pendingEvents, err := h.repo.PendingOutbox(h.ctx, 10, nil)
	must(t, err)
	if len(pendingEvents) < 2 {
		t.Fatalf("expected at least 2 pending outbox events, got %d", len(pendingEvents))
	}
	must(t, h.repo.ConfirmOutbox(h.ctx, pendingEvents[0].ID))
	must(t, h.repo.ConfirmOutbox(h.ctx, pendingEvents[1].ID))
	oldSent := h.clock.Now().Add(-48 * time.Hour)
	newSent := h.clock.Now()
	_, err = h.db.Exec(`UPDATE resources.outbox SET sent_at=$1 WHERE id=$2`, oldSent, pendingEvents[0].ID)
	must(t, err)
	_, err = h.db.Exec(`UPDATE resources.outbox SET sent_at=$1 WHERE id=$2`, newSent, pendingEvents[1].ID)
	must(t, err)

	prunedOutbox, err := h.repo.PruneOutbox(h.ctx, h.clock.Now().Add(-24*time.Hour), 1)
	must(t, err)
	if prunedOutbox != 1 {
		t.Fatalf("expected 1 pruned outbox row, got %d", prunedOutbox)
	}
	var stillThere int
	must(t, h.db.QueryRow(`SELECT count(*) FROM resources.outbox WHERE id=$1`, pendingEvents[1].ID).Scan(&stillThere))
	if stillThere != 1 {
		t.Fatal("recent confirmed outbox row was pruned")
	}

	// PruneJobs deletes completed jobs older than cutoff in batches while preserving pending jobs.
	prunedJobs, err := h.repo.PruneJobs(h.ctx, h.clock.Now(), 1)
	must(t, err)
	if prunedJobs != 1 {
		t.Fatalf("expected batch of 1 done job pruned, got %d", prunedJobs)
	}
	afterJobStats, err := h.s.OperationalStats(h.ctx)
	must(t, err)
	if afterJobStats.PendingJobs != 1 || afterJobStats.DoneJobs != stats.DoneJobs-1 {
		t.Fatalf("pending job affected or done job count mismatch: before=%+v after=%+v", stats, afterJobStats)
	}

	// PruneSubmissionLimits deletes only expired windows while keeping active window.
	_, err = h.s.CreateRequest(h.ctx, reader, pub.ID, "limit-expired", r.Request{Kind: "complaint", Reason: "expired window", Contact: "a@example.invalid", Evidence: []string{"ev"}})
	must(t, err)
	h.clock.Add(2 * time.Minute)
	_, err = h.s.CreateRequest(h.ctx, uploader, pub.ID, "limit-active", r.Request{Kind: "complaint", Reason: "active window", Contact: "b@example.invalid", Evidence: []string{"ev"}})
	must(t, err)

	prunedLimits, err := h.repo.PruneSubmissionLimits(h.ctx, h.clock.Now(), 10)
	must(t, err)
	if prunedLimits != 1 {
		t.Fatalf("expected 1 expired submission limit pruned, got %d", prunedLimits)
	}
	var remainingLimits int
	must(t, h.db.QueryRow(`SELECT count(*) FROM resources.submission_limits`).Scan(&remainingLimits))
	if remainingLimits != 1 {
		t.Fatalf("active submission limit window was pruned: remaining=%d", remainingLimits)
	}
}

func TestPruningUsesCompletionTimeAndSkipsRenewingWindows(t *testing.T) {
	t.Run("long queued job", func(t *testing.T) {
		h := newFixture(t)
		d := h.draft(t, "waited-to-run", "public")
		_, err := h.s.Submit(h.ctx, uploader, d.ID, "queue-now", r.Command{Revision: d.Revision})
		must(t, err)
		h.clock.Add(48 * time.Hour)
		h.drain(t)
		removed, err := h.repo.PruneJobs(h.ctx, h.clock.Now().Add(-24*time.Hour), 10)
		must(t, err)
		if removed != 0 {
			t.Fatalf("recent completion pruned: %d", removed)
		}
		removed, err = h.repo.PruneJobs(h.ctx, h.clock.Now().Add(time.Second), 10)
		must(t, err)
		if removed != 1 {
			t.Fatalf("expired completion not pruned: %d", removed)
		}
	})
	t.Run("existing completed job", func(t *testing.T) {
		h := newFixture(t)
		queued := h.clock.Now().Add(-48 * time.Hour)
		_, err := h.admin.Exec(`INSERT INTO resources.jobs(id,state,available_at,lease_until,data) VALUES('legacy-done','done',$1,$1,'{}')`, queued)
		must(t, err)
		must(t, rp.Migrate(h.ctx, h.admin))
		var completed time.Time
		must(t, h.admin.QueryRow(`SELECT completed_at FROM resources.jobs WHERE id='legacy-done'`).Scan(&completed))
		must(t, rp.Migrate(h.ctx, h.admin))
		var again time.Time
		must(t, h.admin.QueryRow(`SELECT completed_at FROM resources.jobs WHERE id='legacy-done'`).Scan(&again))
		if !again.Equal(completed) {
			t.Fatal("migration reset completion time")
		}
		removed, err := h.repo.PruneJobs(h.ctx, completed, 1)
		must(t, err)
		if removed != 0 {
			t.Fatal("migration prematurely pruned job")
		}
		removed, err = h.repo.PruneJobs(h.ctx, completed.Add(time.Second), 1)
		must(t, err)
		if removed != 1 {
			t.Fatal("legacy job did not expire")
		}
	})
	t.Run("renewing window", func(t *testing.T) {
		h := newFixture(t)
		now := h.clock.Now()
		_, err := h.db.Exec(`INSERT INTO resources.submission_limits(owner_key,started_at,used) VALUES('renewing',$1,1)`, now.Add(-time.Hour))
		must(t, err)
		tx, err := h.db.Begin()
		must(t, err)
		defer tx.Rollback()
		_, err = tx.Exec(`UPDATE resources.submission_limits SET started_at=$1,used=1 WHERE owner_key='renewing'`, now)
		must(t, err)
		ctx, cancel := context.WithTimeout(h.ctx, 2*time.Second)
		defer cancel()
		removed, err := h.repo.PruneSubmissionLimits(ctx, now, 10)
		must(t, err)
		if removed != 0 {
			t.Fatalf("renewing window removed: %d", removed)
		}
		must(t, tx.Commit())
		var used int
		must(t, h.db.QueryRow(`SELECT used FROM resources.submission_limits WHERE owner_key='renewing' AND started_at=$1`, now).Scan(&used))
		if used != 1 {
			t.Fatalf("renewed count=%d", used)
		}
	})
}
