package resources_test

import (
	"errors"
	"fmt"
	r "github.com/StuHelper/stuhelper-resources"
	rp "github.com/StuHelper/stuhelper-resources/postgres"
	"sync"
	"testing"
)

func TestQuotaIsolationKeepsSafetyActionsAvailableAndEnforcesExactCaps(t *testing.T) {
	h := newFixture(t)
	published := h.publish(t, h.draft(t, "quota-pub", "restricted"))
	grant, e := h.s.PutGrant(h.ctx, manager, published.ID, "g1", "grant-init", r.GrantWrite{SubjectKey: reader.Key()})
	must(t, e)
	pendingDraft := h.draft(t, "quota-pending", "public")
	pendingDraft, e = h.s.Submit(h.ctx, uploader, pendingDraft.ID, "submit-pending", r.Command{Revision: pendingDraft.Revision})
	must(t, e)
	extraDraft := h.draft(t, "quota-extra", "public")

	var recCount, opCount, activeJobs int
	must(t, h.admin.QueryRow(`SELECT (SELECT count(*) FROM resources.records),(SELECT count(*) FROM resources.replays),(SELECT count(*) FROM resources.jobs WHERE state IN ('pending','working'))`).Scan(&recCount, &opCount, &activeJobs))
	if activeJobs != 1 {
		t.Fatalf("expected 1 active review job, got %d", activeJobs)
	}

	// Tighten quotas so records, active jobs and operations are all at capacity.
	tight := h.cfg
	tight.MaxRecords = recCount
	tight.MaxJobs = activeJobs
	tight.MaxOperations = opCount
	tightSvc, e := r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}, tight)
	must(t, e)

	// New record creation and new non-emergency operations must be rejected at capacity.
	if _, e = tightSvc.CreateDraft(h.ctx, uploader, "over-rec", r.DraftWrite{Metadata: metadata("public")}); !errors.Is(e, r.ErrCapacity) {
		t.Fatalf("expected ErrCapacity for CreateDraft, got %v", e)
	}
	if _, e = tightSvc.PutGrant(h.ctx, manager, published.ID, "g2", "over-op-grant", r.GrantWrite{SubjectKey: uploader.Key()}); !errors.Is(e, r.ErrCapacity) {
		t.Fatalf("expected ErrCapacity for new grant at MaxOperations, got %v", e)
	}

	// Emergency safety/containment actions must remain available even when all three quotas are full.
	revoked, e := tightSvc.PutGrant(h.ctx, manager, published.ID, grant.ID, "emergency-revoke", r.GrantWrite{Revision: grant.Revision, SubjectKey: reader.Key(), Revoked: true})
	must(t, e)
	if !revoked.Revoked {
		t.Fatal("grant was not revoked", revoked)
	}

	rejected, e := tightSvc.ApplyDecision(h.ctx, manager, pendingDraft.ID, "emergency-reject", r.Decision{ID: "dec-reject", Revision: pendingDraft.Revision, VersionID: pendingDraft.Proposal.ID, Action: "reject", Reason: "违规内容"})
	must(t, e)
	if rejected.Proposal == nil || rejected.Proposal.State != "rejected" {
		t.Fatal("pending proposal was not rejected", rejected)
	}

	unlisted, e := tightSvc.SetState(h.ctx, manager, published.ID, "emergency-unlist", r.StateChange{Revision: published.Revision, State: "unlisted", Reason: "紧急下架"})
	must(t, e)
	if unlisted.State != "unlisted" {
		t.Fatal("resource was not unlisted", unlisted)
	}

	hidden, e := tightSvc.ApplyContentDecision(h.ctx, manager, r.Decision{ID: "mod-hide", Revision: unlisted.Revision, VersionID: unlisted.Current.ID, Action: "hide", Reason: "安全停用"})
	must(t, e)
	if hidden.State != "blocked" {
		t.Fatal("moderation hide did not block resource", hidden)
	}

	// Raise MaxOperations while keeping MaxRecords and MaxJobs saturated:
	// metadata updates without new records/jobs succeed, while queuing a new job fails.
	jobsFull := tight
	jobsFull.MaxOperations = opCount + 50
	jobsFullSvc, e := r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}, jobsFull)
	must(t, e)

	meta := extraDraft.Proposal.Metadata
	meta.Title = "容量满时修正标题"
	extraDraft, e = jobsFullSvc.UpdateDraft(h.ctx, uploader, extraDraft.ID, "edit-under-job-cap", r.DraftWrite{Revision: extraDraft.Revision, Metadata: meta})
	must(t, e)
	if _, e = jobsFullSvc.Submit(h.ctx, uploader, extraDraft.ID, "submit-under-job-cap", r.Command{Revision: extraDraft.Revision}); !errors.Is(e, r.ErrCapacity) {
		t.Fatalf("expected ErrCapacity when MaxJobs saturated, got %v", e)
	}

	// Verify concurrent exact hard cap on MaxRecords and counter consistency after Migrate.
	h2 := newFixture(t)
	capCfg := h2.cfg
	capCfg.MaxRecords = 3
	capSvc, e := r.New(h2.repo, r.Dependencies{Authority: h2.auth, Catalog: h2.catalog, Files: h2.files, Moderation: h2.moderation}, capCfg)
	must(t, e)

	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded, capped := 0, 0
	for i := 0; i < 16; i++ {
		i := i
		wg.Go(func() {
			_, err := capSvc.CreateDraft(h2.ctx, uploader, fmt.Sprintf("race-cap-%d", i), r.DraftWrite{Metadata: metadata("public")})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
			} else if errors.Is(err, r.ErrCapacity) {
				capped++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	if succeeded != 3 || capped != 13 {
		t.Fatalf("concurrent MaxRecords cap violated: succeeded=%d capped=%d", succeeded, capped)
	}
	var actualRecords int
	must(t, h2.admin.QueryRow(`SELECT count(*) FROM resources.records`).Scan(&actualRecords))
	if actualRecords != 3 {
		t.Fatalf("expected 3 persisted records, got %d", actualRecords)
	}
	must(t, rp.Migrate(h2.ctx, h2.admin))
	var txRec, txOps, txJobs int
	must(t, h2.repo.Transaction(h2.ctx, func(tx r.Transaction) error {
		var err error
		txRec, txOps, txJobs, err = tx.Counts()
		return err
	}))
	var rawOps, rawJobs int
	must(t, h2.admin.QueryRow(`SELECT (SELECT count(*) FROM resources.replays),(SELECT count(*) FROM resources.jobs WHERE state IN ('pending','working'))`).Scan(&rawOps, &rawJobs))
	if txRec != actualRecords || txOps != rawOps || txJobs != rawJobs {
		t.Fatalf("counter mismatch: counts=(%d,%d,%d) raw=(%d,%d,%d)", txRec, txOps, txJobs, actualRecords, rawOps, rawJobs)
	}
}
