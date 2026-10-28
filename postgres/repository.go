// Package postgres owns the resources schema and data access identity.
package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	resources "github.com/StuHelper/stuhelper-resources"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"time"
)

//go:embed schema.sql
var Schema string

type Repository struct{ DB *sql.DB }
type transaction struct {
	tx  *sql.Tx
	ctx context.Context
}

func safe(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, sql.ErrNoRows) {
		return resources.ErrNotFound
	}
	if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return e
	}
	var pg *pgconn.PgError
	if errors.As(e, &pg) && (pg.Code == "23505" || pg.Code == "40001" || pg.Code == "40P01") {
		return resources.ErrConflict
	}
	return resources.ErrUnavailable
}
func (r *Repository) Transaction(ctx context.Context, run func(resources.Transaction) error) error {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return safe(e)
	}
	defer tx.Rollback()
	if e = run(&transaction{tx, ctx}); e != nil {
		return e
	}
	return safe(tx.Commit())
}
func (t *transaction) Lock(key string) error {
	_, e := t.tx.ExecContext(t.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "stuhelper-resources:"+key)
	return safe(e)
}
func read[T any](t *transaction, q string, args ...any) (T, error) {
	var v T
	var b []byte
	e := t.tx.QueryRowContext(t.ctx, q, args...).Scan(&b)
	if e != nil {
		return v, safe(e)
	}
	if json.Unmarshal(b, &v) != nil {
		return v, resources.ErrUnavailable
	}
	return v, nil
}
func list[T any](t *transaction, q string, args ...any) ([]T, error) {
	rows, e := t.tx.QueryContext(t.ctx, q, args...)
	if e != nil {
		return nil, safe(e)
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var v T
		var b []byte
		if rows.Scan(&b) != nil || json.Unmarshal(b, &v) != nil {
			return nil, resources.ErrUnavailable
		}
		out = append(out, v)
	}
	return out, safe(rows.Err())
}
func (t *transaction) save(q string, v any, args ...any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return resources.ErrInput
	}
	args = append(args, string(b))
	_, e = t.tx.ExecContext(t.ctx, q, args...)
	return safe(e)
}
func (t *transaction) Category(id string, rev int64) (resources.Category, error) {
	if rev == 0 {
		return read[resources.Category](t, `SELECT data FROM resources.categories WHERE id=$1`, id)
	}
	return read[resources.Category](t, `SELECT data FROM resources.category_revisions WHERE id=$1 AND revision=$2`, id, rev)
}
func (t *transaction) SaveCategory(v resources.Category) error {
	if e := t.save(`INSERT INTO resources.category_revisions(id,revision,data) VALUES($1,$2,$3)`, v, v.ID, v.Revision); e != nil {
		return e
	}
	return t.save(`INSERT INTO resources.categories(id,revision,enabled,data) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,enabled=excluded.enabled,data=excluded.data`, v, v.ID, v.Revision, v.Enabled)
}
func (t *transaction) Categories(n int, after string, disabled bool) ([]resources.Category, error) {
	return list[resources.Category](t, `SELECT data FROM resources.categories WHERE id>$1 AND ($2 OR enabled) ORDER BY id LIMIT $3`, after, disabled, n)
}
func (t *transaction) Record(id string) (resources.Record, error) {
	return read[resources.Record](t, `SELECT data FROM resources.records WHERE id=$1`, id)
}
func (t *transaction) SaveRecord(v resources.Record) error {
	return t.save(`INSERT INTO resources.records(id,owner_key,school_id,state,active_id,proposal_id,data) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(id) DO UPDATE SET state=excluded.state,active_id=excluded.active_id,proposal_id=excluded.proposal_id,data=excluded.data`, v, v.ID, v.Owner.Key(), v.SchoolID, v.State, v.ActiveID, v.ProposalID)
}
func (t *transaction) Version(id string) (resources.VersionRecord, error) {
	return read[resources.VersionRecord](t, `SELECT data FROM resources.versions WHERE id=$1`, id)
}
func (t *transaction) SaveVersion(v resources.VersionRecord) error {
	return t.save(`INSERT INTO resources.versions(id,resource_id,number,data) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, v, v.Version.ID, v.ResourceID, v.Version.Number)
}
func (t *transaction) List(f resources.Filter) ([]resources.Record, error) {
	attrs, e := json.Marshal(f.Attributes)
	if e != nil {
		return nil, resources.ErrInput
	}
	if string(attrs) == "null" {
		attrs = []byte("{}")
	}
	return list[resources.Record](t, `SELECT r.data FROM resources.records r JOIN resources.versions v ON v.id=CASE WHEN $1 THEN coalesce(nullif(r.proposal_id,''),r.active_id) ELSE r.active_id END
 WHERE r.id>$2 AND ($3='' OR r.school_id=$3) AND ($4='' OR r.owner_key=$4) AND ($1 OR r.state='published')
 AND ($5='' OR v.data->'Version'->'metadata'->'associations'->>'course_id'=$5)
 AND ($6='' OR v.data->'Version'->'metadata'->>'category_id'=$6)
 AND ($7='' OR v.data->'Version'->'metadata'->>'directory_id'=$7)
 AND ($8='' OR strpos(lower(v.data->'Version'->'metadata'->>'title'),lower($8))>0)
 AND coalesce(v.data->'Version'->'metadata'->'attributes','{}'::jsonb) @> $9::jsonb
 ORDER BY r.id LIMIT $10`, f.IncludeUnpublished, f.After, f.SchoolID, f.OwnerKey, f.CourseID, f.CategoryID, f.DirectoryID, f.Query, string(attrs), f.Limit)
}
func (t *transaction) Counts() (a, b, c int, e error) {
	e = t.tx.QueryRowContext(t.ctx, `SELECT (SELECT count(*) FROM resources.records),(SELECT count(*) FROM resources.replays),(SELECT count(*) FROM resources.jobs WHERE state IN ('pending','working'))`).Scan(&a, &b, &c)
	return a, b, c, safe(e)
}
func (t *transaction) Replay(key string) (resources.Replay, error) {
	return read[resources.Replay](t, `SELECT data FROM resources.replays WHERE key=$1`, key)
}
func (t *transaction) SaveReplay(v resources.Replay) error {
	return t.save(`INSERT INTO resources.replays(key,data) VALUES($1,$2)`, v, v.Key)
}
func (t *transaction) Grant(resource, subject string) (resources.GrantRecord, error) {
	return read[resources.GrantRecord](t, `SELECT data FROM resources.grants WHERE resource_id=$1 AND subject_key=$2`, resource, subject)
}
func (t *transaction) GrantByID(id string) (resources.GrantRecord, error) {
	return read[resources.GrantRecord](t, `SELECT data FROM resources.grants WHERE id=$1`, id)
}
func (t *transaction) Grants(resource string, n int, after string) ([]resources.GrantRecord, error) {
	return list[resources.GrantRecord](t, `SELECT data FROM resources.grants WHERE resource_id=$1 AND id>$2 ORDER BY id LIMIT $3`, resource, after, n)
}
func (t *transaction) SaveGrant(v resources.GrantRecord) error {
	return t.save(`INSERT INTO resources.grants(id,resource_id,subject_key,data) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, v, v.Grant.ID, v.ResourceID, v.SubjectKey)
}
func (t *transaction) Directory(id string) (resources.Directory, error) {
	return read[resources.Directory](t, `SELECT data FROM resources.directories WHERE id=$1`, id)
}
func (t *transaction) SaveDirectory(v resources.Directory) error {
	return t.save(`INSERT INTO resources.directories(id,school_id,course_id,parent_id,data) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET parent_id=excluded.parent_id,data=excluded.data`, v, v.ID, v.SchoolID, v.CourseID, v.ParentID)
}
func (t *transaction) Directories(school, course, parent string, n int, after string) ([]resources.Directory, error) {
	return list[resources.Directory](t, `SELECT data FROM resources.directories WHERE school_id=$1 AND ($2='' OR course_id=$2) AND parent_id=$3 AND id>$4 ORDER BY id LIMIT $5`, school, course, parent, after, n)
}
func (t *transaction) Package(id string) (resources.PackageRecord, error) {
	return read[resources.PackageRecord](t, `SELECT data FROM resources.packages WHERE id=$1`, id)
}
func (t *transaction) SavePackage(v resources.PackageRecord) error {
	return t.save(`INSERT INTO resources.packages(id,expires_at,data) VALUES($1,$2,$3)`, v, v.Package.ID, v.Package.ExpiresAt)
}
func (t *transaction) Request(id string) (resources.RequestRecord, error) {
	return read[resources.RequestRecord](t, `SELECT data FROM resources.requests WHERE id=$1`, id)
}
func (t *transaction) SaveRequest(v resources.RequestRecord) error {
	return t.save(`INSERT INTO resources.requests(id,resource_id,data) VALUES($1,$2,$3) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, v, v.Receipt.ID, v.ResourceID)
}
func (t *transaction) Job(id string) (resources.Job, error) {
	return read[resources.Job](t, `SELECT data FROM resources.jobs WHERE id=$1`, id)
}
func (t *transaction) SaveJob(v resources.Job) error {
	return t.save(`INSERT INTO resources.jobs(id,state,available_at,lease_until,data) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET state=excluded.state,available_at=excluded.available_at,lease_until=excluded.lease_until,data=excluded.data`, v, v.ID, v.State, v.AvailableAt, v.LeaseUntil)
}
func (t *transaction) Event(v resources.Event) error {
	if e := t.save(`INSERT INTO resources.audit(id,data) VALUES($1,$2)`, v, v.ID); e != nil {
		return e
	}
	return t.save(`INSERT INTO resources.outbox(id,data) VALUES($1,$2)`, v, v.ID)
}
func (r *Repository) Claim(ctx context.Context, worker string, now time.Time, lease time.Duration) (resources.Job, error) {
	var out resources.Job
	e := r.Transaction(ctx, func(port resources.Transaction) error {
		t := port.(*transaction)
		var e error
		out, e = read[resources.Job](t, `SELECT data FROM resources.jobs WHERE (state='pending' AND available_at<=$1) OR (state='working' AND lease_until<=$1) ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`, now)
		if e != nil {
			return e
		}
		out.State = "working"
		out.Owner = worker
		out.Generation++
		out.Attempts++
		out.LeaseUntil = now.Add(lease)
		return t.SaveJob(out)
	})
	return out, e
}
func (r *Repository) PendingEvents(ctx context.Context, n int) ([]resources.Event, error) {
	if n < 1 || n > 100 {
		return nil, resources.ErrInput
	}
	var out []resources.Event
	e := r.Transaction(ctx, func(port resources.Transaction) error {
		var e error
		out, e = list[resources.Event](port.(*transaction), `SELECT data FROM resources.outbox WHERE NOT delivered ORDER BY id LIMIT $1`, n)
		return e
	})
	return out, e
}
func (r *Repository) AcknowledgeEvent(ctx context.Context, id string) error {
	_, e := r.DB.ExecContext(ctx, `UPDATE resources.outbox SET delivered=true WHERE id=$1`, id)
	return safe(e)
}

// PrunePackages removes only expired selection snapshots, never business records or idempotency receipts.
func (r *Repository) PrunePackages(ctx context.Context, now time.Time, n int) error {
	if n < 1 || n > 1000 {
		return resources.ErrInput
	}
	_, e := r.DB.ExecContext(ctx, `DELETE FROM resources.packages WHERE id IN (SELECT id FROM resources.packages WHERE expires_at<$1 ORDER BY expires_at LIMIT $2)`, now, n)
	return safe(e)
}

var _ resources.Repository = (*Repository)(nil)

// CheckRuntime rejects migration credentials and cross-module access at startup.
func (r *Repository) CheckRuntime(ctx context.Context) error {
	var elevated, ownSchema, foreignAccess bool
	e := r.DB.QueryRowContext(ctx, `SELECT rolsuper OR rolcreatedb OR rolcreaterole OR rolbypassrls,
 EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='resources' AND nspowner=(SELECT oid FROM pg_roles WHERE rolname=current_user)),
 EXISTS(SELECT 1 FROM pg_namespace WHERE nspname NOT IN ('resources','public','information_schema') AND nspname NOT LIKE 'pg_%' AND has_schema_privilege(current_user,oid,'USAGE'))
 FROM pg_roles WHERE rolname=current_user`).Scan(&elevated, &ownSchema, &foreignAccess)
	if e != nil || elevated || ownSchema || foreignAccess {
		return resources.ErrDenied
	}
	for _, table := range []string{"audit", "replays", "category_revisions"} {
		var read, insert, change bool
		e = r.DB.QueryRowContext(ctx, `SELECT has_table_privilege($1,'SELECT'),has_table_privilege($1,'INSERT'),has_table_privilege($1,'UPDATE,DELETE')`, "resources."+table).Scan(&read, &insert, &change)
		if e != nil || !read || !insert || change {
			return resources.ErrDenied
		}
	}
	return nil
}
