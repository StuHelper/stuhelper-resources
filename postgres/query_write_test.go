package postgres

import (
	"context"
	"database/sql"
	"fmt"
	m "github.com/StuHelper/stuhelper-resources"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"os"
	"testing"
	"time"
)

func queryWriteDatabase(t *testing.T) (*sql.DB, *pgx.ConnConfig) {
	t.Helper()
	dsn := os.Getenv("STUHELPER_RESOURCES_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	name := fmt.Sprintf("query_write_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE DATABASE ` + name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg.Database = name
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { db.Close(); _, _ = admin.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`); admin.Close() })
	if _, err = db.Exec(Schema); err != nil {
		t.Fatal(err)
	}
	var triggers int
	if err = db.QueryRow(`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='resources' AND p.proname='account_query_changed'`).Scan(&triggers); err != nil || triggers != 0 {
		t.Fatal("unexpected timestamp trigger function", triggers, err)
	}
	return db, cfg
}

func TestAccountQueryRepositoryWritesRefreshTime(t *testing.T) {
	db, _ := queryWriteDatabase(t)
	ctx := context.Background()
	raw, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Rollback()
	tx := &transaction{tx: raw, ctx: ctx}
	record := m.Record{ID: "resource", Owner: m.Principal{Issuer: "issuer", Subject: "owner"}, SchoolID: "school", Revision: 1, State: "draft", CreatedAt: time.Now().UTC()}
	if err = tx.SaveRecord(record); err != nil {
		t.Fatal(err)
	}
	grant := m.GrantRecord{ResourceID: record.ID, SubjectKey: record.Owner.Key(), Grant: m.Grant{ID: "grant", SubjectKey: record.Owner.Key(), Revision: 1}}
	if err = tx.SaveGrant(grant); err != nil {
		t.Fatal(err)
	}
	read := func(table string) time.Time {
		t.Helper()
		var at time.Time
		if e := raw.QueryRowContext(ctx, "SELECT query_changed_at FROM resources."+table).Scan(&at); e != nil {
			t.Fatal(e)
		}
		if at.IsZero() {
			t.Fatal("missing timestamp")
		}
		return at
	}
	for _, state := range []string{"pending", "published", "hidden"} {
		before := read("records")
		record.State = state
		record.Revision++
		if err = tx.SaveRecord(record); err != nil {
			t.Fatal(err)
		}
		if after := read("records"); !after.After(before) {
			t.Fatalf("SaveRecord(%s) did not advance time: %s -> %s", state, before, after)
		}
		saved, e := tx.Record(record.ID)
		if e != nil || saved.State != state {
			t.Fatal(saved, e)
		}
	}
	for _, revoked := range []bool{true, false} {
		before := read("grants")
		grant.Grant.Revoked = revoked
		grant.Grant.Revision++
		expiry := time.Now().UTC().Add(time.Hour)
		grant.Grant.ExpiresAt = &expiry
		if err = tx.SaveGrant(grant); err != nil {
			t.Fatal(err)
		}
		if after := read("grants"); !after.After(before) {
			t.Fatalf("SaveGrant did not advance time: %s -> %s", before, after)
		}
		saved, e := tx.GrantByID(grant.Grant.ID)
		if e != nil || saved.Grant.Revoked != revoked || !saved.Grant.ExpiresAt.Equal(expiry) {
			t.Fatal(saved, e)
		}
	}
	if err = raw.Commit(); err != nil {
		t.Fatal(err)
	}
	// Final-schema initialization preserves both payloads and write timestamps.
	var before string
	if err = db.QueryRow(`SELECT jsonb_agg(to_jsonb(r) ORDER BY id)::text FROM resources.records r`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(Schema); err != nil {
		t.Fatal(err)
	}
	var after string
	if err = db.QueryRow(`SELECT jsonb_agg(to_jsonb(r) ORDER BY id)::text FROM resources.records r`).Scan(&after); err != nil || before != after {
		t.Fatal("initialization changed records", err)
	}
	var savedTime time.Time
	if err = db.QueryRow(`SELECT query_changed_at FROM resources.records WHERE id='resource'`).Scan(&savedTime); err != nil {
		t.Fatal(err)
	}
	rollback := fmt.Errorf("rollback")
	repo := &Repository{DB: db}
	err = repo.Transaction(ctx, func(t m.Transaction) error {
		record.Revision++
		record.State = "deleted"
		if e := t.SaveRecord(record); e != nil {
			return e
		}
		return rollback
	})
	if err != rollback {
		t.Fatal(err)
	}
	var state string
	var at time.Time
	if err = db.QueryRow(`SELECT state,query_changed_at FROM resources.records WHERE id='resource'`).Scan(&state, &at); err != nil || state != "hidden" || !at.Equal(savedTime) {
		t.Fatal("rollback changed data or time", state, at, err)
	}
}
