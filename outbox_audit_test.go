package resources_test

import (
	"encoding/json"
	"fmt"
	r "github.com/StuHelper/stuhelper-resources"
	a "github.com/StuHelper/stuhelper-resources/auditquery"
	"testing"
	"time"
)

func TestOutboxRevisionsSchoolScopedWorkerAuditAndFIFO(t *testing.T) {
	h := newFixture(t)
	// Confirm seed category outbox events before starting scenario.
	seedEvents, e := h.repo.PendingOutbox(h.ctx, 100, nil)
	must(t, e)
	for _, ev := range seedEvents {
		must(t, h.repo.ConfirmOutbox(h.ctx, ev.ID))
	}

	draft := h.draft(t, "oa-item", "restricted")
	published := h.publish(t, draft)
	grant, e := h.s.PutGrant(h.ctx, manager, published.ID, "oa-grant", "oa-grant-k", r.GrantWrite{SubjectKey: reader.Key()})
	must(t, e)
	dir, e := h.s.PutDirectory(h.ctx, manager, "oa-dir", "oa-dir-k", r.DirectoryWrite{SchoolID: "school", CourseID: "course", Names: r.Names{"zh-CN": "讲义"}, Enabled: true})
	must(t, e)
	cov, e := h.s.PutCoverage(h.ctx, global, "oa-plan", "oa-cov-k", r.Coverage{PlanID: "oa-plan", Rules: []r.CoverageRule{{SchoolID: "school"}}})
	must(t, e)
	pkg, e := h.s.CreatePackage(h.ctx, reader, "oa-pkg-k", r.PackageRequest{ResourceIDs: []string{published.ID}})
	must(t, e)
	_, e = h.s.CreateRequest(h.ctx, uploader, published.ID, "oa-req-k", r.Request{Kind: "withdrawal", Reason: "补充说明"})
	must(t, e)
	h.drain(t)

	// A-07.1 & A-07.3: Every business outbox event carries a positive revision matching its domain object,
	// and package event Subject equals pkg.ID.
	events, e := h.repo.PendingOutbox(h.ctx, 100, nil)
	must(t, e)
	sawPackageOutbox := false
	sawPublishCompleted := false
	for _, ev := range events {
		if ev.Type == "resources.discovery.v1" {
			must(t, h.repo.ConfirmOutbox(h.ctx, ev.ID))
			continue
		}
		var payload struct {
			Revision int64 `json:"revision"`
		}
		must(t, json.Unmarshal(ev.Data, &payload))
		if payload.Revision < 1 {
			t.Fatalf("event %s (subject=%s) emitted non-positive revision %d", ev.Type, ev.Subject, payload.Revision)
		}
		switch ev.Type {
		case "resources.grants.manage.v1":
			if payload.Revision != grant.Revision {
				t.Fatalf("grant event revision=%d want=%d", payload.Revision, grant.Revision)
			}
		case "resources.directories.manage.v1":
			if payload.Revision != dir.Revision {
				t.Fatalf("directory event revision=%d want=%d", payload.Revision, dir.Revision)
			}
		case "resources.coverage.manage.v1":
			if payload.Revision != cov.Revision {
				t.Fatalf("coverage event revision=%d want=%d", payload.Revision, cov.Revision)
			}
		case "resources.package.v1":
			sawPackageOutbox = true
			if ev.Subject != pkg.ID || payload.Revision != 1 {
				t.Fatalf("package event subject=%s rev=%d want subject=%s rev=1", ev.Subject, payload.Revision, pkg.ID)
			}
		case "resources.publish.completed.v1":
			sawPublishCompleted = true
			if payload.Revision != published.Revision {
				t.Fatalf("publish completed revision=%d want=%d", payload.Revision, published.Revision)
			}
		}
		must(t, h.repo.ConfirmOutbox(h.ctx, ev.ID))
	}
	if !sawPackageOutbox || !sawPublishCompleted {
		t.Fatalf("missing expected events: package=%v publishCompleted=%v", sawPackageOutbox, sawPublishCompleted)
	}

	// A-07.2: School-scoped administrator can read worker completion audit for their school.
	until := h.clock.Now().Add(time.Minute)
	schoolScopes := []a.Scope{{School: "school", Group: "*", Module: "resources"}}
	pubAudit, e := h.repo.ReadAudit(h.ctx, a.Request{Scopes: schoolScopes, Target: published.ID, Action: "resources.publish.completed.v1", Until: until, Limit: 10})
	must(t, e)
	if len(pubAudit.Records) != 1 || pubAudit.Records[0].Scope.School != "school" {
		t.Fatalf("school admin cannot see publish.completed audit: %+v", pubAudit.Records)
	}

	// A-07.3: Package creation audit records pkg.ID as Target (not 'unknown') and school scope when uniform.
	pkgAudit, e := h.repo.ReadAudit(h.ctx, a.Request{Scopes: schoolScopes, Target: pkg.ID, Action: "resources.package.v1", Until: until, Limit: 10})
	must(t, e)
	if len(pkgAudit.Records) != 1 || pkgAudit.Records[0].Target != pkg.ID {
		t.Fatalf("package audit target/scope mismatch: %+v", pkgAudit.Records)
	}

	// A-15: PendingOutbox preserves global write order (seq ASC) across subjects, preventing lexicographical starvation.
	must(t, h.repo.Transaction(h.ctx, func(tx r.Transaction) error {
		if err := tx.Event(r.Event{ID: "early-zz", Type: "resources.test.v1", ResourceID: "zz-tail-subject", SchoolID: "school", Actor: "tester", Revision: 1, Time: h.clock.Now()}); err != nil {
			return err
		}
		for i := 0; i < 100; i++ {
			if err := tx.Event(r.Event{ID: fmt.Sprintf("flood-%03d", i), Type: "resources.test.v1", ResourceID: "00-head-subject", SchoolID: "school", Actor: "tester", Revision: int64(i + 1), Time: h.clock.Now()}); err != nil {
				return err
			}
		}
		return nil
	}))
	batch, e := h.repo.PendingOutbox(h.ctx, 100, nil)
	must(t, e)
	if len(batch) != 100 || batch[0].ID != "early-zz" || batch[0].Subject != "zz-tail-subject" {
		t.Fatalf("zz-tail-subject was starved by lexicographically earlier subject: first=%+v", batch[0])
	}
}
