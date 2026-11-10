package resources_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	r "github.com/StuHelper/stuhelper-resources"
	aq "github.com/StuHelper/stuhelper-resources/accountquery"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"os"
	"sync"
	"testing"
	"time"
)

func (h *fixture) billing(t *testing.T, id, typ, subject string, data any) error {
	t.Helper()
	raw, e := json.Marshal(data)
	must(t, e)
	return h.s.Consume(h.ctx, r.Envelope{DataContentType: "application/json", SpecVersion: "1.0", ID: id, Source: "billing", Type: typ, Subject: subject, Time: h.clock.Now(), Data: raw})
}
func (h *fixture) purchase(t *testing.T, id, order string, rev int, refund bool) {
	t.Helper()
	typ := "billing.resource-unlock.paid.v1"
	if refund {
		typ = "billing.resource-unlock.refunded.v1"
	}
	must(t, h.billing(t, fmt.Sprintf("%s-%d", order, rev), typ, order, map[string]any{"order_id": order, "owner": reader, "resource_id": id, "revision": rev}))
}
func TestEntitlementSourcesCoverageExpiryRefundAndEligibility(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "rights", "restricted"))
	decision := func(p r.Principal, id, want string) {
		t.Helper()
		got, e := h.s.UnlockEligibility(h.ctx, p, id)
		must(t, e)
		if got.Reason != want || got.Eligible != (want == "eligible") {
			t.Fatal(got, want)
		}
	}
	download := func(ok bool) {
		t.Helper()
		_, e := h.s.Download(h.ctx, reader, item.ID, &bytes.Buffer{})
		if ok {
			must(t, e)
		} else if !errors.Is(e, r.ErrDenied) {
			t.Fatal(e)
		}
	}
	decision(reader, item.ID, "eligible")
	decision(manager, item.ID, "already_accessible")
	decision(r.Principal{}, item.ID, "unauthenticated")
	decision(reader, "absent", "not_found")
	h.auth.set(reader.Key()+":purchase", true)
	decision(reader, item.ID, "requirements_missing")
	h.auth.set(reader.Key()+":purchase", false)
	public := h.publish(t, h.draft(t, "free", "public"))
	decision(reader, public.ID, "already_accessible")
	h.auth.set(reader.Key()+":download.public", true)
	decision(reader, public.ID, "not_purchasable")
	h.auth.set(reader.Key()+":download.public", false)
	c, e := h.s.PutCoverage(h.ctx, global, "plan", "coverage", r.Coverage{PlanID: "plan", Rules: []r.CoverageRule{{SchoolID: "school", CategoryID: item.Current.Metadata.CategoryID}}})
	must(t, e)
	_, e = h.s.PutCoverage(h.ctx, reader, "plan", "unauthorized", c)
	if !errors.Is(e, r.ErrDenied) {
		t.Fatal(e)
	}
	m := r.Membership{Owner: reader, PlanID: "plan", ValidFrom: h.clock.Now(), ValidUntil: h.clock.Now().Add(time.Hour), Revision: 2}
	must(t, h.billing(t, "membership-2", "billing.membership.changed.v1", "member", m))
	download(true)
	decision(reader, item.ID, "already_accessible")
	c.Rules = []r.CoverageRule{}
	c, e = h.s.PutCoverage(h.ctx, global, "plan", "remove-coverage", c)
	must(t, e)
	download(false)
	c.Rules = []r.CoverageRule{{SchoolID: "school"}}
	_, e = h.s.PutCoverage(h.ctx, global, "plan", "restore-coverage", c)
	must(t, e)
	download(true)
	m.Revision = 1
	m.ValidUntil = h.clock.Now()
	must(t, h.billing(t, "membership-old", "billing.membership.changed.v1", "member", m))
	download(true)
	grant, e := h.s.PutGrant(h.ctx, manager, item.ID, "direct", "direct", r.GrantWrite{SubjectKey: reader.Key()})
	must(t, e)
	h.purchase(t, item.ID, "order", 1, false)
	h.purchase(t, item.ID, "order", 1, false)
	download(true)
	h.purchase(t, item.ID, "order", 2, true)
	download(true)
	_, e = h.s.PutGrant(h.ctx, manager, item.ID, "direct", "revoke", r.GrantWrite{SubjectKey: reader.Key(), Revision: grant.Revision, Revoked: true})
	must(t, e)
	download(true)
	h.clock.Add(time.Hour)
	download(false)
	// Old paid revision cannot resurrect a refunded source, even with a new event ID.
	must(t, h.billing(t, "old-paid", "billing.resource-unlock.paid.v1", "order", map[string]any{"order_id": "order", "owner": reader, "resource_id": item.ID, "revision": 1}))
	download(false)
	h.purchase(t, item.ID, "second-order", 1, false)
	download(true)
	h.auth.set(reader.Key()+":download.restricted", true)
	download(false)
	h.auth.set(reader.Key()+":download.restricted", false)
	download(true)
	rows, e := h.repo.ReadAccount(h.ctx, aq.Selection{Query: aq.Query{Value: reader.Key(), Limit: 100}, Scopes: []aq.Scope{{School: "school", Group: "*", Module: "resources"}}})
	must(t, e)
	found := false
	for _, row := range rows {
		if row.ID == "resources:unlock:second-order" && row.Current {
			found = true
		}
	}
	if !found {
		t.Fatal(rows)
	}
	var count int
	must(t, h.db.QueryRow(`SELECT count(*) FROM resources.outbox WHERE data->>'type'='resources.entitlement.granted.v1'`).Scan(&count))
	if count != 2 {
		t.Fatal(count)
	}
}
func TestEntitlementRejectedAtomicInboxAndValidation(t *testing.T) {
	h := newFixture(t)
	for _, state := range []string{"unlisted", "blocked"} {
		item := h.publish(t, h.draft(t, state, "restricted"))
		_, e := h.s.SetState(h.ctx, manager, item.ID, state, r.StateChange{Revision: item.Revision, State: state, Reason: "fixture"})
		must(t, e)
		h.purchase(t, item.ID, state, 1, false)
		d, e := h.s.UnlockEligibility(h.ctx, reader, item.ID)
		must(t, e)
		if d.Reason != "resource_unavailable" {
			t.Fatal(d)
		}
	}
	var count int
	must(t, h.db.QueryRow(`SELECT count(*) FROM resources.outbox WHERE data->>'type'='resources.entitlement.rejected.v1'`).Scan(&count))
	if count != 2 {
		t.Fatal(count)
	}
	item := h.publish(t, h.draft(t, "atomic", "restricted"))
	data := map[string]any{"order_id": "atomic", "owner": reader, "resource_id": item.ID, "revision": 1}
	must(t, h.admin.QueryRow(`SELECT current_user`).Scan(new(string)))
	// Force outbox failure through a transaction-scoped database constraint.
	_, e := h.admin.Exec(`ALTER TABLE resources.outbox ADD CONSTRAINT reject_entitlement CHECK(data->>'type'<>'resources.entitlement.granted.v1')`)
	must(t, e)
	if h.billing(t, "atomic-event", "billing.resource-unlock.paid.v1", "atomic", data) == nil {
		t.Fatal("expected rollback")
	}
	must(t, h.db.QueryRow(`SELECT count(*) FROM resources.inbox WHERE id='atomic-event'`).Scan(&count))
	if count != 0 {
		t.Fatal("inbox committed")
	}
	must(t, h.db.QueryRow(`SELECT count(*) FROM resources.purchases WHERE id='atomic'`).Scan(&count))
	if count != 0 {
		t.Fatal("purchase committed")
	}
	_, e = h.admin.Exec(`ALTER TABLE resources.outbox DROP CONSTRAINT reject_entitlement`)
	must(t, e)
	must(t, h.billing(t, "atomic-event", "billing.resource-unlock.paid.v1", "atomic", data))
	data["unexpected"] = "rejected"
	if !errors.Is(h.billing(t, "bad", "billing.resource-unlock.paid.v1", "atomic", data), r.ErrInput) {
		t.Fatal("unknown field accepted")
	}
}

func TestFourAccessSourceCombinations(t *testing.T) {
	h := newFixture(t)
	for mask := 0; mask < 16; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			item := h.publish(t, h.draft(t, fmt.Sprintf("combination-%d", mask), "restricted"))
			if mask&2 != 0 {
				_, e := h.s.PutGrant(h.ctx, manager, item.ID, fmt.Sprintf("grant-%d", mask), fmt.Sprintf("grant-key-%d", mask), r.GrantWrite{SubjectKey: reader.Key()})
				must(t, e)
			}
			if mask&4 != 0 {
				plan := fmt.Sprintf("plan-%d", mask)
				_, e := h.s.PutCoverage(h.ctx, global, plan, plan, r.Coverage{PlanID: plan, Rules: []r.CoverageRule{{SchoolID: "school"}}})
				must(t, e)
				must(t, h.billing(t, plan, "billing.membership.changed.v1", plan, r.Membership{Owner: reader, PlanID: plan, ValidFrom: h.clock.Now(), ValidUntil: h.clock.Now().Add(time.Minute), Revision: 1}))
			}
			if mask&8 != 0 {
				h.purchase(t, item.ID, fmt.Sprintf("order-%d", mask), 1, false)
			}
			s, e := r.New(h.repo, r.Dependencies{Authority: discoveryAuthority{func(ctx context.Context, p r.Principal, action, scope string) error {
				if p == reader && action == "manage" && scope == "school" && mask&1 != 0 {
					return nil
				}
				return h.auth.Check(ctx, p, action, scope)
			}}, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}, h.cfg)
			must(t, e)
			_, e = s.Download(h.ctx, reader, item.ID, &bytes.Buffer{})
			if mask != 0 {
				must(t, e)
				d, e := s.UnlockEligibility(h.ctx, reader, item.ID)
				must(t, e)
				if d.Eligible {
					t.Fatal("would charge an authorized reader")
				}
			} else if !errors.Is(e, r.ErrDenied) {
				t.Fatal(e)
			}
			h.clock.Add(2 * time.Minute)
		})
	}
}

func TestConcurrentBillingDeliveryAndEventSchemas(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "concurrent-billing", "restricted"))
	payload := map[string]any{"order_id": "concurrent", "owner": reader, "resource_id": item.ID, "revision": 1}
	raw, e := json.Marshal(payload)
	must(t, e)
	event := r.Envelope{DataContentType: "application/json", SpecVersion: "1.0", ID: "concurrent-event", Source: "billing", Type: "billing.resource-unlock.paid.v1", Subject: "concurrent", Time: h.clock.Now(), Data: raw}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Go(func() { errs <- h.s.Consume(h.ctx, event) })
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		must(t, e)
	}
	var count int
	must(t, h.db.QueryRow(`SELECT count(*) FROM resources.purchases WHERE id='concurrent'`).Scan(&count))
	if count != 1 {
		t.Fatal(count)
	}
	must(t, h.db.QueryRow(`SELECT count(*) FROM resources.outbox WHERE data->>'type'='resources.entitlement.granted.v1'`).Scan(&count))
	if count != 1 {
		t.Fatal(count)
	}
	events, e := h.repo.PendingOutbox(h.ctx, 100, nil)
	must(t, e)
	events = append(events, event)
	for _, event := range events {
		if event.Type != "resources.entitlement.granted.v1" && event.Type != "billing.resource-unlock.paid.v1" {
			continue
		}
		raw, e := os.ReadFile("contracts/" + event.Type + ".schema.json")
		must(t, e)
		var doc any
		must(t, json.Unmarshal(raw, &doc))
		compiler := jsonschema.NewCompiler()
		must(t, compiler.AddResource("urn:event", doc))
		schema, e := compiler.Compile("urn:event")
		must(t, e)
		raw, e = json.Marshal(event)
		must(t, e)
		var value any
		must(t, json.Unmarshal(raw, &value))
		must(t, schema.Validate(value))
	}
}

func TestPurchaseAndDownloadConditionsAreIndependent(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "purchase-rules", "restricted"))
	for _, purchaseDenied := range []bool{false, true} {
		for _, downloadDenied := range []bool{false, true} {
			h.auth.set(reader.Key()+":purchase", purchaseDenied)
			h.auth.set(reader.Key()+":download.restricted", downloadDenied)
			d, err := h.s.UnlockEligibility(h.ctx, reader, item.ID)
			must(t, err)
			want := "eligible"
			if purchaseDenied {
				want = "requirements_missing"
			}
			if d.Reason != want || d.Eligible == purchaseDenied {
				t.Fatalf("purchase denied=%v download denied=%v: %+v", purchaseDenied, downloadDenied, d)
			}
			view, err := h.s.Get(h.ctx, reader, item.ID)
			must(t, err)
			if view.UnlockReason != want {
				t.Fatal(view)
			}
		}
	}
	h.purchase(t, item.ID, "independent", 1, false)
	// Purchase restrictions do not revoke existing rights; download restrictions pause delivery.
	for _, downloadDenied := range []bool{false, true} {
		h.auth.set(reader.Key()+":download.restricted", downloadDenied)
		_, err := h.s.Download(h.ctx, reader, item.ID, &bytes.Buffer{})
		if downloadDenied {
			if !errors.Is(err, r.ErrDenied) {
				t.Fatal(err)
			}
		} else {
			must(t, err)
		}
	}
	var n int
	must(t, h.db.QueryRow("SELECT count(*) FROM resources.purchases WHERE id='independent' AND data->>'revoked'='false'").Scan(&n))
	if n != 1 {
		t.Fatal("qualification change removed purchase", n)
	}
}
