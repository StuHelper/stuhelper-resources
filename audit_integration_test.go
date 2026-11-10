package resources_test

import (
	resources "github.com/StuHelper/stuhelper-resources"
	a "github.com/StuHelper/stuhelper-resources/auditquery"
	"testing"
	"time"
)

func TestAuditWritesPreserveObjectAndOperationReason(t *testing.T) {
	h := newFixture(t)
	draft := h.draft(t, "audit-object", "public")
	q := a.Request{Scopes: []a.Scope{{School: "*", Group: "*", Module: "resources"}}, Target: draft.ID, Action: "resources.upload.v1", Until: h.clock.Now().Add(time.Second), Limit: 10}
	page, e := h.repo.ReadAudit(h.ctx, q)
	must(t, e)
	// The fixture creates a draft and confirms its file: both operations own the same object.
	if len(page.Records) != 2 || page.Records[0].Reason != "" || page.Records[0].Result != "success" {
		t.Fatal(page)
	}
	item := h.publish(t, draft)
	_, e = h.s.SetState(h.ctx, manager, item.ID, "audit-unlist", resources.StateChange{Revision: item.Revision, State: "unlisted", Reason: "reviewed"})
	must(t, e)
	q.Action = "resources.manage.v1"
	q.Until = h.clock.Now().Add(time.Second)
	page, e = h.repo.ReadAudit(h.ctx, q)
	must(t, e)
	if len(page.Records) != 1 || page.Records[0].Reason != "reviewed" || page.Records[0].Result != "success" {
		t.Fatal(page)
	}
	var exposed int
	must(t, h.db.QueryRow(`SELECT count(*) FROM resources.outbox WHERE data ? 'Reason'`).Scan(&exposed))
	if exposed != 0 {
		t.Fatal("audit reason entered outbox", exposed)
	}
}
