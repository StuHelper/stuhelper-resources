package resources_test

import (
	"context"
	"errors"
	r "github.com/StuHelper/stuhelper-resources"
	rp "github.com/StuHelper/stuhelper-resources/postgres"
	"slices"
	"testing"
)

type legacyDirectoryRepo struct{ r.Repository }
type legacyDirectoryTx struct{ r.Transaction }

func (repo legacyDirectoryRepo) Transaction(ctx context.Context, run func(r.Transaction) error) error {
	return repo.Repository.Transaction(ctx, func(tx r.Transaction) error { return run(legacyDirectoryTx{tx}) })
}

func TestCategoryOrderDirectoryAdminAndCoverageScope(t *testing.T) {
	h := newFixture(t)
	must(t, rp.Migrate(h.ctx, h.admin))

	// A-12.1: Categories must be ordered by configured Order ASC, ID COLLATE "C" ASC across pages.
	gotCats := []string{}
	after := ""
	for {
		page, err := h.s.ReadCategories(h.ctx, reader, 2, after, false)
		must(t, err)
		for _, item := range page.Items {
			gotCats = append(gotCats, item.ID)
		}
		if page.NextCursor == "" {
			break
		}
		after = page.NextCursor
	}
	wantCats := []string{"exam", "courseware", "past-papers", "textbooks", "exercises"}
	if !slices.Equal(gotCats, wantCats) {
		t.Fatalf("categories order/pagination mismatch: got %v want %v", gotCats, wantCats)
	}

	// A-12.2 & A-12.3: Ordinary users only see enabled directories; directories.manage admins can read
	// all school directories (including disabled ones) without student discover eligibility.
	_, e := h.s.PutDirectory(h.ctx, manager, "dir-enabled", "k-dir-en", r.DirectoryWrite{SchoolID: "school", CourseID: "course", Names: r.Names{"zh-CN": "启用目录"}, Enabled: true})
	must(t, e)
	_, e = h.s.PutDirectory(h.ctx, manager, "dir-disabled", "k-dir-dis", r.DirectoryWrite{SchoolID: "school", CourseID: "course", Names: r.Names{"zh-CN": "停用目录"}, Enabled: false})
	must(t, e)

	readerDirs, e := h.s.Directories(h.ctx, reader, "school", "course", "", 25, "")
	must(t, e)
	if len(readerDirs.Items) != 1 || readerDirs.Items[0].ID != "dir-enabled" {
		t.Fatalf("ordinary reader saw disabled directory: %+v", readerDirs.Items)
	}

	legacy, err := r.New(legacyDirectoryRepo{h.repo}, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}, h.cfg)
	must(t, err)
	first, err := legacy.Directories(h.ctx, reader, "school", "course", "", 1, "")
	must(t, err)
	if len(first.Items) != 1 || first.Items[0].ID != "dir-enabled" {
		t.Fatalf("enabled directory missing from legacy page: %+v", first)
	}

	h.auth.set("discover", true)
	adminDirs, e := h.s.Directories(h.ctx, manager, "school", "course", "", 25, "")
	must(t, e)
	if len(adminDirs.Items) != 2 || adminDirs.Items[0].ID != "dir-disabled" || adminDirs.Items[1].ID != "dir-enabled" {
		t.Fatalf("scoped directory administrator failed to list all directories without discover: %+v", adminDirs.Items)
	}
	if _, e = h.s.Directories(h.ctx, manager, "other-school", "", "", 25, ""); !errors.Is(e, r.ErrDenied) {
		t.Fatalf("expected ErrDenied for out-of-scope school directory admin, got %v", e)
	}
	if _, e = h.s.Directories(h.ctx, reader, "school", "course", "", 25, ""); !errors.Is(e, r.ErrDenied) {
		t.Fatalf("expected ErrDenied for unqualified reader, got %v", e)
	}
	h.auth.set("discover", false)

	// A-14: GetCoverage verifies discover permission against every SchoolID in Coverage.Rules for non-managers,
	// while coverage.manage administrators retain cross-school read access.
	_, e = h.s.PutCoverage(h.ctx, global, "plan-local", "cov-local", r.Coverage{PlanID: "plan-local", Rules: []r.CoverageRule{{SchoolID: "school", CategoryID: "exam"}}})
	must(t, e)
	// Seed a foreign-school coverage rule directly (representing a multi-school plan in shared storage).
	_, e = h.admin.Exec(`INSERT INTO resources.coverage(id,data) VALUES('plan-foreign','{"plan_id":"plan-foreign","revision":1,"rules":[{"school_id":"other-school","category_id":"exam"}]}'::jsonb)`)
	must(t, e)

	localCov, e := h.s.GetCoverage(h.ctx, reader, "plan-local")
	must(t, e)
	if localCov.PlanID != "plan-local" {
		t.Fatal(localCov)
	}
	if _, e = h.s.GetCoverage(h.ctx, reader, "plan-foreign"); !errors.Is(e, r.ErrDenied) {
		t.Fatalf("foreign school coverage leaked to single-school reader: err=%v", e)
	}
	foreignAdminCov, e := h.s.GetCoverage(h.ctx, global, "plan-foreign")
	must(t, e)
	if foreignAdminCov.PlanID != "plan-foreign" {
		t.Fatal(foreignAdminCov)
	}
}
