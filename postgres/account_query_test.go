package postgres

import (
	"context"
	"database/sql"
	"fmt"
	aq "github.com/StuHelper/stuhelper-resources/accountquery"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"os"
	"testing"
	"time"
)

func TestAccountQueryScopePaginationAndRuntime(t *testing.T) {
	dsn := os.Getenv("STUHELPER_RESOURCES_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	ctx := context.Background()
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	admin, e := sql.Open("pgx", cfg.ConnString())
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	name := fmt.Sprintf("iq_%d", time.Now().UnixNano())
	if _, e = admin.Exec(`CREATE DATABASE ` + name); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
	cfg.Database = name
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	if _, e = db.Exec(Schema); e != nil {
		t.Fatal(e)
	}
	exec := func(s string, args ...any) {
		t.Helper()
		if _, e := db.Exec(s, args...); e != nil {
			t.Fatal(e)
		}
	}
	target := aq.Principal{Issuer: "issuer", Subject: "target"}
	for _, v := range []struct{ id, school string }{{"a", "other"}, {"b", "school"}, {"c", "school"}} {
		exec(`INSERT INTO resources.records(id,owner_key,school_id,state,active_id,proposal_id,data) VALUES($1,$2,$3,'published','','','{}')`, v.id, target.Key(), v.school)
	}
	exec(`INSERT INTO resources.grants(id,resource_id,subject_key,data) VALUES('grant','c',$1,'{"Grant":{"revoked":false}}')`, target.Key())
	// Repeating final-schema initialization must preserve stored records.
	if _, e = db.Exec(Schema); e != nil {
		t.Fatal(e)
	}
	role := name + "_reader"
	if _, e = admin.Exec(`CREATE ROLE ` + role + ` LOGIN`); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(`DROP ROLE ` + role)
	defer admin.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
	exec(`GRANT USAGE ON SCHEMA resources TO ` + role)
	exec(`GRANT SELECT ON ALL TABLES IN SCHEMA resources TO ` + role)
	cfg.User = role
	runtimeDB := stdlib.OpenDB(*cfg)
	defer runtimeDB.Close()
	if _, e = runtimeDB.Exec(`DELETE FROM resources.records`); e == nil {
		t.Fatal("reader modified source")
	}
	repo := &Repository{DB: runtimeDB}
	scope := aq.Scope{School: "school", Group: "*", Module: aq.Domain}

	s := aq.Selection{Query: aq.Query{Kind: "account", Value: target.Key(), Limit: 1}, Scopes: []aq.Scope{scope}, SensitiveScopes: []aq.Scope{}}
	ids := map[string]bool{}
	for {
		page, e := repo.ReadAccount(ctx, s)
		if e != nil {
			t.Fatal(e)
		}
		if len(page) == 0 {
			break
		}
		if len(page) != 1 || ids[page[0].ID] || !scope.Contains(page[0].Scope) {
			t.Fatal("scope or page", page)
		}
		ids[page[0].ID] = true
		s.Query.After = page[0].ID
	}
	if len(ids) != 3 {
		t.Fatal("unexpected filtered records", ids)
	}
	s.Query.After = ""
	s.Scopes = []aq.Scope{{School: "school", Group: "ungranted", Module: aq.Domain}}
	page, e := repo.ReadAccount(ctx, s)
	if e != nil || len(page) != 0 {
		t.Fatal("group scope bypass", page, e)
	}

}
