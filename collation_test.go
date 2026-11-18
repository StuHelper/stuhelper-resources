package resources_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	r "github.com/StuHelper/stuhelper-resources"
	rp "github.com/StuHelper/stuhelper-resources/postgres"
)

type flexibleCatalog struct{}

func (flexibleCatalog) Validate(_ context.Context, a r.Associations) error {
	if a.SchoolID != "school" {
		return r.ErrInput
	}
	return nil
}

func (flexibleCatalog) ReferenceNames(_ context.Context, ids []string) (r.References, error) {
	out := r.References{Items: make([]r.Reference, 0, len(ids)), MissingIDs: []string{}}
	for _, id := range ids {
		out.Items = append(out.Items, r.Reference{ID: id, Kind: "course", Name: "Name " + id, CanonicalID: id})
	}
	return out, nil
}

func newUTF8CollationFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("STUHELPER_RESOURCES_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	control, err := sql.Open("pgx", dsn)
	must(t, err)
	var b [8]byte
	_, err = rand.Read(b[:])
	must(t, err)
	name := "resources_utf8_" + hex.EncodeToString(b[:])
	role := name + "_runtime"
	_, err = control.Exec(`CREATE DATABASE ` + name + ` TEMPLATE template0 LOCALE_PROVIDER icu ICU_LOCALE 'en-US' ENCODING 'UTF8'`)
	must(t, err)
	admin, err := sql.Open("pgx", dsn+" dbname="+name)
	must(t, err)
	_, err = admin.Exec(rp.Schema)
	must(t, err)
	_, err = admin.Exec(fmt.Sprintf(`CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
 GRANT USAGE ON SCHEMA resources TO %s; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA resources TO %s;
 REVOKE UPDATE,DELETE ON resources.audit,resources.replays,resources.category_revisions,resources.inbox FROM %s;
 REVOKE ALL ON SCHEMA public FROM PUBLIC;`, role, role, role, role))
	must(t, err)
	db, err := sql.Open("pgx", dsn+" dbname="+name+" user="+role)
	must(t, err)
	t.Cleanup(func() {
		db.Close()
		admin.Close()
		_, _ = control.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
		_, _ = control.Exec(`DROP ROLE ` + role)
		control.Close()
	})

	// Confirm the database default collation differs from ASCII C byte order.
	rows, err := admin.Query(`SELECT x FROM unnest(ARRAY['CS_101','course-1','cs-101']) x ORDER BY x`)
	must(t, err)
	var defaultOrder []string
	for rows.Next() {
		var s string
		must(t, rows.Scan(&s))
		defaultOrder = append(defaultOrder, s)
	}
	rows.Close()
	if slices.Equal(defaultOrder, []string{"CS_101", "course-1", "cs-101"}) {
		t.Fatalf("expected non-C default collation order, got %v", defaultOrder)
	}

	h := &fixture{
		repo:       &rp.Repository{DB: db},
		db:         db,
		admin:      admin,
		clock:      &testClock{t: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		auth:       &authority{deny: map[string]bool{}},
		catalog:    &catalog{},
		files:      &files{items: map[string]r.FileEvidence{}, refs: map[string]string{}, released: map[string]bool{}},
		moderation: &moderation{items: map[string]r.ReviewSubmission{}},
		ctx:        context.Background(),
	}
	must(t, h.repo.CheckRuntime(h.ctx))
	h.cfg = r.Config{
		Clock:             h.clock.Now,
		Terms:             r.Terms{Version: "fixture-v1", Digest: strings.Repeat("a", 64), URL: "https://example.invalid/terms"},
		AllowedTypes:      []string{"application/pdf"},
		MaxFileSize:       100,
		MaxPackageBytes:   100,
		MaxPackageFiles:   2,
		MaxPackageParts:   3,
		MaxDirectoryDepth: 3,
		MaxRecords:        1000,
		MaxOperations:     10000,
		MaxJobs:           1000,
		OperationTimeout:  time.Second,
		PackageTTL:        time.Hour,
		Lease:             time.Minute,
		MaxAttempts:       3,
	}
	h.s, err = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: flexibleCatalog{}, Files: h.files, Moderation: h.moderation}, h.cfg)
	must(t, err)
	_, err = h.s.PutCategory(h.ctx, global, "exam", "seed", r.CategoryWrite{Names: r.Names{"zh-CN": "往年题"}, Enabled: true, Fields: []r.Field{}})
	must(t, err)
	return h
}

func TestUTF8CollationPaginationAndOwnedReferences(t *testing.T) {
	h := newUTF8CollationFixture(t)

	// 1. OwnedReferences with mixed-case and hyphenated IDs that sort differently in UTF-8 vs C.
	courseIDs := []string{"cs-101", "CS_101", "course1", "course-1", "course10", "course-2"}
	for i, cid := range courseIDs {
		meta := r.Metadata{
			Title:            "Collation " + cid,
			Description:      "UTF-8 test",
			Tags:             []string{},
			CategoryID:       "exam",
			CategoryRevision: 1,
			Attributes:       map[string]r.Value{},
			Associations:     r.Associations{SchoolID: "school", CourseID: cid},
			Access:           "public",
			Origin:           r.Origin{Kind: "unknown"},
		}
		key := fmt.Sprintf("utf8-%d", i)
		draft, err := h.s.CreateDraft(h.ctx, uploader, key, r.DraftWrite{Metadata: meta})
		must(t, err)
		file := r.File{VersionID: "file-" + key, Filename: "doc.pdf", ContentType: "application/pdf", Size: 60, SHA256: strings.Repeat("b", 64)}
		h.files.mu.Lock()
		h.files.items[file.VersionID] = r.FileEvidence{File: file, Owner: uploader, Module: "resources", SchoolID: "school", State: "awaiting_approval", Access: "public"}
		h.files.mu.Unlock()
		draft, err = h.s.AttachFile(h.ctx, uploader, draft.ID, "att-"+key, r.FileLink{Revision: draft.Revision, MediaVersionID: file.VersionID})
		must(t, err)
		h.publish(t, draft)
	}

	refs, err := h.s.MyReferences(h.ctx, uploader, courseIDs)
	must(t, err)
	if len(refs.Items) != len(courseIDs) {
		t.Fatalf("expected %d references, got %+v", len(courseIDs), refs)
	}

	// 2. Exclusive cursor pagination across Categories and Directories with mixed-case & hyphenated IDs.
	mixedIDs := []string{"Dir_A", "dir-a", "dir-1", "dir1", "dir10", "dir-2"}
	for _, id := range mixedIDs {
		_, err := h.s.PutCategory(h.ctx, global, id, "cat-"+id, r.CategoryWrite{Names: r.Names{"zh-CN": id}, Enabled: true, Fields: []r.Field{}})
		must(t, err)
		_, err = h.s.PutDirectory(h.ctx, manager, id, "dir-"+id, r.DirectoryWrite{SchoolID: "school", CourseID: "course-1", Names: r.Names{"zh-CN": id}, Enabled: true})
		must(t, err)
	}

	seenDirs := []string{}
	afterDir := ""
	for range 20 {
		page, err := h.s.Directories(h.ctx, reader, "school", "course-1", "", 1, afterDir)
		must(t, err)
		if len(page.Items) == 0 {
			break
		}
		seenDirs = append(seenDirs, page.Items[0].ID)
		if page.NextCursor == "" {
			break
		}
		afterDir = page.NextCursor
	}
	wantDirs := slices.Clone(mixedIDs)
	slices.Sort(wantDirs)
	if !slices.Equal(seenDirs, wantDirs) {
		t.Fatalf("directory pagination under UTF-8 collation mismatch: got %v, want %v", seenDirs, wantDirs)
	}

	// 3. Resource list pagination (both sort=id and sort=newest) visits all 6 published items without duplicates.
	for _, sortMode := range []string{"id", "newest"} {
		seenRes := map[string]bool{}
		after := ""
		for range 20 {
			page, err := h.s.List(h.ctx, reader, r.Filter{SchoolID: "school", Sort: sortMode, Limit: 1, After: after}, false, false)
			must(t, err)
			for _, item := range page.Items {
				if seenRes[item.ID] {
					t.Fatalf("duplicate resource %s in %s pagination", item.ID, sortMode)
				}
				seenRes[item.ID] = true
			}
			if page.NextCursor == "" {
				break
			}
			after = page.NextCursor
		}
		if len(seenRes) != len(courseIDs) {
			t.Fatalf("sort=%s visited %d resources, want %d", sortMode, len(seenRes), len(courseIDs))
		}
	}
}
