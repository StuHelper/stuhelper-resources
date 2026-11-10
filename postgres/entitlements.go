package postgres

import (
	m "github.com/StuHelper/stuhelper-resources"
	"time"
)

func (t *transaction) Coverage(id string) (m.Coverage, error) {
	return read[m.Coverage](t, `SELECT data FROM resources.coverage WHERE id=$1`, id)
}
func (t *transaction) SaveCoverage(v m.Coverage) error {
	return t.save(`INSERT INTO resources.coverage(id,data) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, v, v.PlanID)
}
func (t *transaction) Purchase(id string) (m.PurchaseSource, error) {
	return read[m.PurchaseSource](t, `SELECT data FROM resources.purchases WHERE id=$1`, id)
}
func (t *transaction) SavePurchase(v m.PurchaseSource) error {
	return t.save(`INSERT INTO resources.purchases(id,owner_key,resource_id,data) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET data=excluded.data,query_changed_at=clock_timestamp()`, v, v.OrderID, v.Owner.Key(), v.ResourceID)
}
func (t *transaction) Membership(owner, plan string) (m.Membership, error) {
	return read[m.Membership](t, `SELECT data FROM resources.memberships WHERE owner_key=$1 AND plan_id=$2`, owner, plan)
}
func (t *transaction) SaveMembership(v m.Membership) error {
	return t.save(`INSERT INTO resources.memberships(owner_key,plan_id,data) VALUES($1,$2,$3) ON CONFLICT(owner_key,plan_id) DO UPDATE SET data=excluded.data`, v, v.Owner.Key(), v.PlanID)
}
func (t *transaction) Inbox(id string) (bool, error) {
	var exists bool
	e := t.tx.QueryRowContext(t.ctx, `SELECT EXISTS(SELECT 1 FROM resources.inbox WHERE id=$1)`, id).Scan(&exists)
	return exists, safe(e)
}
func (t *transaction) SaveInbox(id string) error {
	_, e := t.tx.ExecContext(t.ctx, `INSERT INTO resources.inbox(id) VALUES($1)`, id)
	return safe(e)
}
func (t *transaction) Envelope(v m.Envelope) error { return t.saveEnvelope(v) }
func (t *transaction) Rights(owner, id, school, directory, category string, now time.Time) (m.Entitlement, error) {
	var out m.Entitlement
	e := t.tx.QueryRowContext(t.ctx, `SELECT
 EXISTS(SELECT 1 FROM resources.purchases WHERE owner_key=$1 AND resource_id=$2 AND data->>'revoked'='false'),
 EXISTS(SELECT 1 FROM resources.memberships m JOIN resources.coverage c ON c.id=m.plan_id CROSS JOIN LATERAL jsonb_array_elements(c.data->'rules') rule
 WHERE m.owner_key=$1 AND (m.data->>'valid_from')::timestamptz<=$6 AND (m.data->>'valid_until')::timestamptz>$6
 AND rule->>'school_id'=$3 AND (COALESCE(rule->>'directory_id','')='' OR rule->>'directory_id'=$4) AND (COALESCE(rule->>'category_id','')='' OR rule->>'category_id'=$5))`, owner, id, school, directory, category, now).Scan(&out.Purchased, &out.Member)
	return out, safe(e)
}
