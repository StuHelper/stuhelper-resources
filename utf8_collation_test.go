package resources_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	r "github.com/StuHelper/stuhelper-resources"
	rp "github.com/StuHelper/stuhelper-resources/postgres"
	"os"
	"strings"
	"testing"
	"time"
)

type unicodeCatalog struct{}

func (unicodeCatalog) Validate(_ context.Context, a r.Associations) error {
	if a.SchoolID != "school" {
		return r.ErrInput
	}
	return nil
}

func (unicodeCatalog) ReferenceNames(_ context.Context, ids []string) (r.References, error) {
	out := r.References{Items: []r.Reference{}, MissingIDs: []string{}}
	for _, id := range ids {
		out.Items = append(out.Items, r.Reference{ID: id, Kind: "course", Name: "Name " + id, CanonicalID: id})
	}
	return out, nil
}

func TestUTF8CollationDatabasePreservesByteOrderForReferencesAndPagination(t *testing.T) {
	dsn := os.Getenv("STUHELPER_RESOURCES_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	control, e := sql.Open("pgx", dsn)
	must(t, e)
	var b [8]byte
	_, e = rand.Read(b[:])
	must(t, e)
	name := "resources_icu_" + hex.EncodeToString(b[:])
	role := name + "_runtime"
	_, e = control.Exec(`CREATE DATABASE ` + name + ` TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER icu ICU_LOCALE 'en-US-u-ka-shifted'`)
	if e != nil {
		control.Close()
		t.Fatalf("create ICU UTF-8 database: %v", e)
	}
	admin, e := sql.Open("pgx", dsn+" dbname="+name)
	must(t, e)
	_, e = admin.Exec(rp.Schema)
	must(t, e)
	_, e = admin.Exec(fmt.Sprintf(`CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
 GRANT USAGE ON SCHEMA resources TO %s; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA resources TO %s;
 REVOKE UPDATE,DELETE ON resources.audit,resources.replays,resources.category_revisions,resources.inbox FROM %s;
 REVOKE ALL ON SCHEMA public FROM PUBLIC;`, role, role, role, role))
	must(t, e)
	db, e := sql.Open("pgx", dsn+" dbname="+name+" user="+role)
	must(t, e)
	t.Cleanup(func() {
		db.Close()
		admin.Close()
		_, _ = control.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
		_, _ = control.Exec(`DROP ROLE ` + role)
		control.Close()
	})

	repo := &rp.Repository{DB: db}
	ctx := context.Background()
	must(t, repo.CheckRuntime(ctx))
	clock := &testClock{t: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)}
	auth := &authority{deny: map[string]bool{}}
	f := &files{items: map[string]r.FileEvidence{}, refs: map[string]string{}, released: map[string]bool{}}
	mod := &moderation{items: map[string]r.ReviewSubmission{}}
	cfg := r.Config{Clock: clock.Now, Terms: r.Terms{Version: "v1", Digest: strings.Repeat("a", 64), URL: "https://example.invalid/terms"}, AllowedTypes: []string{"application/pdf"}, MaxFileSize: 100, MaxPackageBytes: 100, MaxPackageFiles: 2, MaxPackageParts: 3, MaxDirectoryDepth: 3, MaxRecords: 1000, MaxOperations: 10000, MaxJobs: 1000, OperationTimeout: time.Second, PackageTTL: time.Hour, Lease: time.Minute, MaxAttempts: 3}
	svc, e := r.New(repo, r.Dependencies{Authority: auth, Catalog: unicodeCatalog{}, Files: f, Moderation: mod}, cfg)
	must(t, e)

	_, e = svc.PutCategory(ctx, global, "exam", "seed", r.CategoryWrite{Names: r.Names{"zh-CN": "往年题"}, Enabled: true})
	must(t, e)

	for _, cid := range []string{"course-1", "course1", "CS-101", "cs-101"} {
		meta := r.Metadata{Title: "资料 " + cid, Description: "说明", Tags: []string{}, CategoryID: "exam", CategoryRevision: 1, Associations: r.Associations{SchoolID: "school", CourseID: cid}, Access: "public", Origin: r.Origin{Kind: "unknown"}}
		_, e = svc.CreateDraft(ctx, uploader, "draft-"+cid, r.DraftWrite{Metadata: meta})
		must(t, e)
	}

	refs, e := svc.MyReferences(ctx, uploader, []string{"course1", "course-1", "cs-101", "CS-101"})
	if e != nil {
		t.Fatalf("MyReferences failed under UTF-8/ICU collation: %v", e)
	}
	if len(refs.Items) != 4 {
		t.Fatalf("expected 4 owned references, got %+v", refs)
	}

	// Verify Directory pagination in byte order ("Dir-A" < "dir-a", "dir-1" < "dir1").
	for _, did := range []string{"dir1", "dir-1", "dir-a", "Dir-A"} {
		_, e = svc.PutDirectory(ctx, manager, did, "put-"+did, r.DirectoryWrite{SchoolID: "school", Names: r.Names{"zh-CN": did}, Enabled: true})
		must(t, e)
	}
	seenDirs := []string{}
	after := ""
	for {
		page, err := svc.Directories(ctx, reader, "school", "", "", 2, after)
		must(t, err)
		for _, d := range page.Items {
			seenDirs = append(seenDirs, d.ID)
		}
		if page.NextCursor == "" {
			break
		}
		after = page.NextCursor
	}
	wantDirs := []string{"Dir-A", "dir-1", "dir-a", "dir1"}
	if strings.Join(seenDirs, ",") != strings.Join(wantDirs, ",") {
		t.Fatalf("directory byte-order pagination mismatch: got %v want %v", seenDirs, wantDirs)
	}
}
