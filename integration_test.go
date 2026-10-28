package resources_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	resources "github.com/StuHelper/stuhelper-resources"
	rp "github.com/StuHelper/stuhelper-resources/postgres"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var uploader = resources.Principal{Issuer: "urn:fixture", Subject: "uploader"}
var reader = resources.Principal{Issuer: "urn:fixture", Subject: "reader"}
var manager = resources.Principal{Issuer: "urn:fixture", Subject: "manager"}
var global = resources.Principal{Issuer: "urn:fixture", Subject: "global"}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type authority struct {
	mu   sync.Mutex
	deny map[string]bool
}

func (a *authority) Check(_ context.Context, p resources.Principal, action, scope string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deny[action] || a.deny[p.Key()+":"+action] {
		return resources.ErrDenied
	}
	if action == "taxonomy.manage" {
		if p == global {
			return nil
		}
		return resources.ErrDenied
	}
	if action == "manage" || action == "grants.manage" || action == "directories.manage" || action == "moderation.execute" {
		if (p == manager || p == global) && scope == "school" {
			return nil
		}
		return resources.ErrDenied
	}
	if !p.Valid() {
		return resources.ErrUnauthenticated
	}
	if scope != "" && scope != "school" {
		return resources.ErrDenied
	}
	return nil
}
func (a *authority) set(key string, v bool) { a.mu.Lock(); a.deny[key] = v; a.mu.Unlock() }

type catalog struct {
	mu    sync.Mutex
	calls int
}

func (c *catalog) Validate(_ context.Context, a resources.Associations) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if a.SchoolID != "school" || a.CourseID != "" && a.CourseID != "course" {
		return resources.ErrInput
	}
	return nil
}

type files struct {
	mu        sync.Mutex
	items     map[string]resources.FileEvidence
	refs      map[string]string
	writes    int
	fail      bool
	downloads int
	released  map[string]bool
}

func (f *files) Inspect(_ context.Context, _ resources.Principal, id string) (resources.FileEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.items[id]
	if !ok {
		return v, resources.ErrNotFound
	}
	return v, nil
}
func (f *files) Publish(_ context.Context, in resources.Publication) (string, bool, error) {
	id, key := in.MediaVersionID, in.OperationID
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := f.refs[key]
	if ref == "" {
		ref = "ref-" + id + "-" + key[:8]
		f.refs[key] = ref
		f.writes++
	}
	if f.fail {
		return "", false, resources.ErrUnavailable
	}
	return ref, true, nil
}
func (f *files) Release(_ context.Context, id, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released[id] = true
	return nil
}
func (f *files) Download(_ context.Context, _ resources.Principal, id string, w io.Writer) error {
	f.mu.Lock()
	f.downloads++
	f.mu.Unlock()
	_, e := io.WriteString(w, id)
	return e
}
func (f *files) Archive(_ context.Context, _ resources.Principal, items []resources.Selection, w io.Writer) error {
	f.mu.Lock()
	f.downloads++
	f.mu.Unlock()
	for _, item := range items {
		if _, e := io.WriteString(w, item.ResourceID+"\n"); e != nil {
			return e
		}
	}
	return nil
}

type moderation struct {
	mu       sync.Mutex
	items    map[string]resources.ReviewSubmission
	requests int
}

func (m *moderation) Submit(_ context.Context, key string, in resources.ReviewSubmission) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[key] = in
	return "task-" + key[:12], nil
}
func (m *moderation) Request(_ context.Context, key, id string, _ resources.Principal, in resources.Request) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests++
	return "request-" + key[:12], nil
}

type entitlements struct {
	mu     sync.Mutex
	rights map[string]resources.Entitlement
	fail   bool
	calls  int
}

func (e *entitlements) Lookup(_ context.Context, p resources.Principal, id string) (resources.Entitlement, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.fail {
		return resources.Entitlement{}, resources.ErrUnavailable
	}
	return e.rights[p.Key()+":"+id], nil
}
func (e *entitlements) set(p resources.Principal, id string, r resources.Entitlement) {
	e.mu.Lock()
	e.rights[p.Key()+":"+id] = r
	e.mu.Unlock()
}

type fixture struct {
	s          *resources.Service
	repo       *rp.Repository
	db, admin  *sql.DB
	clock      *testClock
	auth       *authority
	catalog    *catalog
	files      *files
	moderation *moderation
	rights     *entitlements
	cfg        resources.Config
	ctx        context.Context
}

func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("STUHELPER_RESOURCES_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	control, e := sql.Open("pgx", dsn)
	must(t, e)
	var b [8]byte
	_, e = rand.Read(b[:])
	must(t, e)
	name := "resources_test_" + hex.EncodeToString(b[:])
	role := name + "_runtime"
	_, e = control.Exec(`CREATE DATABASE ` + name)
	must(t, e)
	admin, e := sql.Open("pgx", dsn+" dbname="+name)
	must(t, e)
	_, e = admin.Exec(rp.Schema)
	must(t, e)
	_, e = admin.Exec(fmt.Sprintf(`CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
 GRANT USAGE ON SCHEMA resources TO %s; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA resources TO %s;
 REVOKE UPDATE,DELETE ON resources.audit,resources.replays,resources.category_revisions FROM %s;
 REVOKE ALL ON SCHEMA public FROM PUBLIC; CREATE SCHEMA foreign_module; CREATE TABLE foreign_module.private (id text); REVOKE ALL ON SCHEMA foreign_module FROM PUBLIC;`, role, role, role, role))
	must(t, e)
	db, e := sql.Open("pgx", dsn+" dbname="+name+" user="+role)
	must(t, e)
	db.SetMaxOpenConns(12)
	t.Cleanup(func() {
		db.Close()
		admin.Close()
		_, e := control.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
		if e != nil {
			t.Error(e)
		}
		_, e = control.Exec(`DROP ROLE ` + role)
		if e != nil {
			t.Error(e)
		}
		control.Close()
	})
	h := &fixture{repo: &rp.Repository{DB: db}, db: db, admin: admin, clock: &testClock{t: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)}, auth: &authority{deny: map[string]bool{}}, catalog: &catalog{}, files: &files{items: map[string]resources.FileEvidence{}, refs: map[string]string{}, released: map[string]bool{}}, moderation: &moderation{items: map[string]resources.ReviewSubmission{}}, rights: &entitlements{rights: map[string]resources.Entitlement{}}, ctx: context.Background()}
	must(t, h.repo.CheckRuntime(h.ctx))
	if e := (&rp.Repository{DB: admin}).CheckRuntime(h.ctx); !errors.Is(e, resources.ErrDenied) {
		t.Fatal("migration identity accepted", e)
	}
	h.cfg = resources.Config{Clock: h.clock.Now, Terms: resources.Terms{Version: "fixture-v1", Digest: strings.Repeat("a", 64), URL: "https://example.invalid/terms"}, AllowedTypes: []string{"application/pdf"}, MaxFileSize: 100, MaxPackageBytes: 100, MaxPackageFiles: 2, MaxPackageParts: 3, MaxDirectoryDepth: 3, MaxRecords: 1000, MaxOperations: 10000, MaxJobs: 1000, OperationTimeout: time.Second, PackageTTL: time.Hour, Lease: time.Minute, MaxAttempts: 3}
	h.s, e = resources.New(h.repo, resources.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation, Entitlements: h.rights}, h.cfg)
	must(t, e)
	min, max := int64(1900), int64(2100)
	_, e = h.s.PutCategory(h.ctx, global, "exam", "seed", resources.CategoryWrite{Names: resources.Names{"zh-CN": "往年题"}, Enabled: true, Fields: []resources.Field{{Key: "year", Names: resources.Names{"zh-CN": "年份"}, Kind: "integer", Min: &min, Max: &max, Filterable: true, Groupable: true}}, GroupBy: "year"})
	must(t, e)
	return h
}
func metadata(access string) resources.Metadata {
	year := int64(2025)
	return resources.Metadata{Title: "离散数学", Description: "学习资料", Tags: []string{}, CategoryID: "exam", CategoryRevision: 1, Attributes: map[string]resources.Value{"year": {Number: &year}}, Associations: resources.Associations{SchoolID: "school", CourseID: "course"}, Access: access, Origin: resources.Origin{Kind: "unknown"}}
}
func (h *fixture) draft(t *testing.T, key, access string) resources.Resource {
	t.Helper()
	r, e := h.s.CreateDraft(h.ctx, uploader, key, resources.DraftWrite{Metadata: metadata(access)})
	must(t, e)
	file := resources.File{VersionID: "file-" + key, Filename: "原始讲义.pdf", ContentType: "application/pdf", Size: 60, SHA256: strings.Repeat("b", 64)}
	h.files.mu.Lock()
	h.files.items[file.VersionID] = resources.FileEvidence{File: file, Owner: uploader, Module: "resources", SchoolID: "school", State: "awaiting_approval", Access: access}
	h.files.mu.Unlock()
	r, e = h.s.AttachFile(h.ctx, uploader, r.ID, "file", resources.FileLink{Revision: r.Revision, MediaVersionID: file.VersionID})
	must(t, e)
	return r
}
func (h *fixture) drain(t *testing.T) {
	t.Helper()
	for n := 0; n < 20; n++ {
		ok, e := h.s.ProcessOne(h.ctx, "worker")
		must(t, e)
		if !ok {
			return
		}
	}
	t.Fatal("worker did not drain")
}
func (h *fixture) publish(t *testing.T, r resources.Resource) resources.Resource {
	t.Helper()
	r, e := h.s.Submit(h.ctx, uploader, r.ID, fmt.Sprintf("submit-%d", r.Revision), resources.Command{Revision: r.Revision})
	must(t, e)
	h.drain(t)
	r, e = h.s.ApplyDecision(h.ctx, manager, r.ID, fmt.Sprintf("approve-%d", r.Revision), resources.Decision{ID: fmt.Sprintf("decision-%d", r.Revision), Revision: r.Revision, VersionID: r.Proposal.ID, Action: "approve"})
	must(t, e)
	h.drain(t)
	r, e = h.s.Get(h.ctx, uploader, r.ID)
	must(t, e)
	if r.Current == nil || r.Proposal != nil {
		t.Fatal("publication not activated")
	}
	return r
}

func TestConfigurableTaxonomySnapshotsAndYearFilters(t *testing.T) {
	h := newFixture(t)
	first := h.publish(t, h.draft(t, "first", "public"))
	page, e := h.s.List(h.ctx, reader, resources.Filter{SchoolID: "school", CategoryID: "exam", Attributes: metadata("public").Attributes, Limit: 25}, false, false)
	must(t, e)
	if len(page.Items) != 1 || page.Items[0].ID != first.ID {
		t.Fatal(page)
	}
	old, e := h.s.Categories(h.ctx, 25, "", true)
	must(t, e)
	category := old.Items[0]
	renamed, e := h.s.PutCategory(h.ctx, global, "exam", "rename", resources.CategoryWrite{Revision: 1, Names: resources.Names{"zh-CN": "历年试卷"}, Enabled: true, Fields: category.Fields, GroupBy: "year"})
	must(t, e)
	unchanged, e := h.s.Get(h.ctx, reader, first.ID)
	must(t, e)
	if unchanged.Current.Category.Names["zh-CN"] != "往年题" || unchanged.Current.Category.Revision != 1 {
		t.Fatal("published snapshot changed")
	}
	if _, e = h.s.CreateDraft(h.ctx, uploader, "stale", resources.DraftWrite{Metadata: metadata("public")}); !errors.Is(e, resources.ErrConflict) {
		t.Fatal(e)
	}
	meta := metadata("public")
	meta.CategoryRevision = renamed.Revision
	meta.Attributes = nil
	r, e := h.s.CreateDraft(h.ctx, uploader, "optional-year", resources.DraftWrite{Metadata: meta})
	must(t, e)
	if len(r.Proposal.Metadata.Attributes) != 0 {
		t.Fatal("fabricated unknown year")
	}
	bad := int64(2200)
	meta.Attributes = map[string]resources.Value{"year": {Number: &bad}}
	if _, e = h.s.CreateDraft(h.ctx, uploader, "bad-year", resources.DraftWrite{Metadata: meta}); !errors.Is(e, resources.ErrInput) {
		t.Fatal(e)
	}
	meta.Attributes = map[string]resources.Value{"undeclared": {Number: &bad}}
	if _, e = h.s.CreateDraft(h.ctx, uploader, "unknown-field", resources.DraftWrite{Metadata: meta}); !errors.Is(e, resources.ErrInput) {
		t.Fatal(e)
	}
	if _, e = h.s.PutCategory(h.ctx, manager, "exam", "school-admin", resources.CategoryWrite{Revision: 2, Names: resources.Names{"zh-CN": "伪造"}, Enabled: true}); !errors.Is(e, resources.ErrDenied) {
		t.Fatal(e)
	}
}
func TestPublicationContinuityAndStaleReview(t *testing.T) {
	h := newFixture(t)
	r := h.publish(t, h.draft(t, "original", "public"))
	old := r.Current.ID
	meta := metadata("public")
	meta.Title = "新标题"
	r, e := h.s.UpdateDraft(h.ctx, uploader, r.ID, "edit", resources.DraftWrite{Revision: r.Revision, Metadata: meta})
	must(t, e)
	r, e = h.s.Submit(h.ctx, uploader, r.ID, "submit-edited", resources.Command{Revision: r.Revision})
	must(t, e)
	staleID, staleRev := r.Proposal.ID, r.Revision
	visible, e := h.s.Get(h.ctx, reader, r.ID)
	must(t, e)
	if visible.Current.ID != old || visible.Proposal != nil || !visible.DownloadAllowed {
		t.Fatal("pending proposal replaced active publication")
	}
	meta.Title = "重新更正"
	r, e = h.s.UpdateDraft(h.ctx, uploader, r.ID, "amend-pending", resources.DraftWrite{Revision: r.Revision, Metadata: meta})
	must(t, e)
	if _, e = h.s.ApplyDecision(h.ctx, manager, r.ID, "stale-review", resources.Decision{ID: "old", Revision: staleRev, VersionID: staleID, Action: "approve"}); !errors.Is(e, resources.ErrConflict) {
		t.Fatal(e)
	}
	r = h.publish(t, r)
	if r.Current.ID == old || r.Current.Metadata.Title != "重新更正" {
		t.Fatal(r)
	}
	h.files.mu.Lock()
	released := len(h.files.released)
	h.files.mu.Unlock()
	if released != 1 {
		t.Fatal("old reference not released", released)
	}
}
func TestIndependentRightsCampusRevocationAndUnlisting(t *testing.T) {
	h := newFixture(t)
	r := h.publish(t, h.draft(t, "restricted", "restricted"))
	var output bytes.Buffer
	if _, e := h.s.Download(h.ctx, reader, r.ID, &output); !errors.Is(e, resources.ErrDenied) {
		t.Fatal(e)
	}
	expires := h.clock.Now().Add(time.Minute)
	grant, e := h.s.PutGrant(h.ctx, manager, r.ID, "manual", "grant", resources.GrantWrite{SubjectKey: reader.Key(), ExpiresAt: &expires})
	must(t, e)
	h.rights.fail = true
	_, e = h.s.Download(h.ctx, reader, r.ID, &output)
	must(t, e)
	h.rights.fail = false
	h.auth.set(reader.Key()+":download.restricted", true)
	if _, e = h.s.Download(h.ctx, reader, r.ID, &output); !errors.Is(e, resources.ErrDenied) {
		t.Fatal(e)
	}
	h.auth.set(reader.Key()+":download.restricted", false)
	h.rights.set(reader, r.ID, resources.Entitlement{Purchased: true})
	h.clock.Add(2 * time.Minute)
	_, e = h.s.Download(h.ctx, reader, r.ID, &output)
	must(t, e)
	_, e = h.s.PutGrant(h.ctx, manager, r.ID, "manual", "revoke", resources.GrantWrite{Revision: grant.Revision, SubjectKey: reader.Key(), Revoked: true})
	must(t, e)
	r, e = h.s.SetState(h.ctx, manager, r.ID, "unlist", resources.StateChange{Revision: r.Revision, State: "unlisted", Reason: "内容整理"})
	must(t, e)
	_, e = h.s.Download(h.ctx, reader, r.ID, &output)
	must(t, e)
	h.rights.set(reader, r.ID, resources.Entitlement{Member: true})
	if _, e = h.s.Download(h.ctx, reader, r.ID, &output); !errors.Is(e, resources.ErrDenied) {
		t.Fatal(e)
	}
	h.rights.set(reader, r.ID, resources.Entitlement{Purchased: true})
	_, e = h.s.SetState(h.ctx, manager, r.ID, "block", resources.StateChange{Revision: r.Revision, State: "blocked", Reason: "安全问题"})
	must(t, e)
	if _, e = h.s.Download(h.ctx, reader, r.ID, &output); !errors.Is(e, resources.ErrDenied) {
		t.Fatal(e)
	}
}
func TestConcurrentIdempotencyAndChangedPayload(t *testing.T) {
	h := newFixture(t)
	input := resources.DraftWrite{Metadata: metadata("public")}
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	for n := 0; n < 12; n++ {
		wg.Go(func() {
			r, e := h.s.CreateDraft(h.ctx, uploader, "same", input)
			if e != nil {
				t.Error(e)
				return
			}
			ids <- r.ID
		})
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("duplicate resource")
		}
	}
	var count int
	must(t, h.admin.QueryRow(`SELECT count(*) FROM resources.records`).Scan(&count))
	if count != 1 {
		t.Fatal(count)
	}
	input.Metadata.Title = "different"
	if _, e := h.s.CreateDraft(h.ctx, uploader, "same", input); !errors.Is(e, resources.ErrConflict) {
		t.Fatal(e)
	}
	must(t, h.admin.QueryRow(`SELECT count(*) FROM resources.audit`).Scan(&count))
	if count != 2 {
		t.Fatal("duplicate audit", count)
	}
}
func TestDeniedInputsDoNotReachDependencies(t *testing.T) {
	h := newFixture(t)
	h.auth.set("upload", true)
	if _, e := h.s.CreateDraft(h.ctx, uploader, "denied", resources.DraftWrite{Metadata: metadata("public")}); !errors.Is(e, resources.ErrDenied) {
		t.Fatal(e)
	}
	if h.catalog.calls != 0 {
		t.Fatal("denied request called catalog")
	}
	if _, e := h.s.CreateDraft(h.ctx, resources.Principal{}, "anonymous", resources.DraftWrite{Metadata: metadata("public")}); !errors.Is(e, resources.ErrUnauthenticated) {
		t.Fatal(e)
	}
}
func TestDirectoryPackagesOmitUnauthorizedAndRecheckRights(t *testing.T) {
	h := newFixture(t)
	root, e := h.s.PutDirectory(h.ctx, manager, "root", "root", resources.DirectoryWrite{SchoolID: "school", CourseID: "course", Names: resources.Names{"zh-CN": "课程"}, Enabled: true})
	must(t, e)
	child, e := h.s.PutDirectory(h.ctx, manager, "child", "child", resources.DirectoryWrite{SchoolID: "school", CourseID: "course", ParentID: "root", Names: resources.Names{"zh-CN": "期末"}, Enabled: true})
	must(t, e)
	if _, e = h.s.PutDirectory(h.ctx, manager, "root", "cycle", resources.DirectoryWrite{Revision: root.Revision, SchoolID: "school", CourseID: "course", ParentID: child.ID, Names: root.Names, Enabled: true}); !errors.Is(e, resources.ErrInput) {
		t.Fatal(e)
	}
	ids := []string{}
	for n, access := range []string{"public", "public", "restricted"} {
		r := h.draft(t, fmt.Sprintf("dir-%d", n), access)
		meta := r.Proposal.Metadata
		meta.DirectoryID = "child"
		r, e = h.s.UpdateDraft(h.ctx, uploader, r.ID, "directory", resources.DraftWrite{Revision: r.Revision, Metadata: meta})
		must(t, e)
		r = h.publish(t, r)
		ids = append(ids, r.ID)
	}
	bundle, e := h.s.CreatePackage(h.ctx, reader, "package", resources.PackageRequest{DirectoryID: "root"})
	must(t, e)
	if len(bundle.Parts) != 2 {
		t.Fatal(bundle)
	}
	for _, part := range bundle.Parts {
		if len(part.Files) != 1 {
			t.Fatal(part)
		}
	}
	var output bytes.Buffer
	must(t, h.s.DownloadPackage(h.ctx, reader, bundle.ID, 1, &output))
	if strings.Contains(output.String(), ids[2]) {
		t.Fatal("unauthorized resource included")
	}
	h.auth.set(reader.Key()+":download.public", true)
	if _, e = h.s.GetPackage(h.ctx, reader, bundle.ID); !errors.Is(e, resources.ErrDenied) {
		t.Fatal(e)
	}
	if _, e = h.s.CreatePackage(h.ctx, reader, "package", resources.PackageRequest{DirectoryID: "root"}); e == nil {
		t.Fatal("idempotency replay leaked revoked manifest")
	}
}
func TestOriginConsentAndRequestsUseOutbox(t *testing.T) {
	h := newFixture(t)
	meta := metadata("public")
	meta.Origin = resources.Origin{Kind: "known", Source: "老师的课堂讲义"}
	r, e := h.s.CreateDraft(h.ctx, uploader, "known", resources.DraftWrite{Metadata: meta})
	must(t, e)
	meta.Origin = resources.Origin{Kind: "original"}
	if _, e = h.s.CreateDraft(h.ctx, uploader, "no-consent", resources.DraftWrite{Metadata: meta}); !errors.Is(e, resources.ErrInput) {
		t.Fatal(e)
	}
	meta.Origin.Consent = &resources.Consent{Accepted: true, Version: h.cfg.Terms.Version, Digest: h.cfg.Terms.Digest}
	_, e = h.s.CreateDraft(h.ctx, uploader, "consent", resources.DraftWrite{Metadata: meta})
	must(t, e)
	request, e := h.s.CreateRequest(h.ctx, uploader, r.ID, "withdraw", resources.Request{Kind: "withdrawal", Reason: "资料更正"})
	must(t, e)
	if request.State != "pending" || h.moderation.requests != 0 {
		t.Fatal("request bypassed outbox")
	}
	h.drain(t)
	status, e := h.s.RequestStatus(h.ctx, uploader, request.ID)
	must(t, e)
	if status.State != "waiting" || status.TaskID == "" {
		t.Fatal("missing request status", status)
	}
	if _, e = h.s.RequestStatus(h.ctx, reader, request.ID); !errors.Is(e, resources.ErrNotFound) {
		t.Fatal("request leaked", e)
	}
	receipts, e := h.s.Requests(h.ctx, uploader, 1, "")
	must(t, e)
	if len(receipts.Items) != 1 || receipts.Items[0].ID != request.ID || receipts.NextCursor != request.ID {
		t.Fatal("receipt listing", receipts)
	}
	other, e := h.s.Requests(h.ctx, reader, 25, "")
	must(t, e)
	if len(other.Items) != 0 {
		t.Fatal("receipt leaked to reader")
	}
	rest, e := h.s.Requests(h.ctx, uploader, 1, receipts.NextCursor)
	must(t, e)
	if len(rest.Items) != 0 {
		t.Fatal("receipt cursor duplicated row")
	}
	if h.moderation.requests != 1 {
		t.Fatal("request not delivered")
	}
	repeated, e := h.s.CreateRequest(h.ctx, uploader, r.ID, "withdraw", resources.Request{Kind: "withdrawal", Reason: "资料更正"})
	must(t, e)
	if repeated.ID != request.ID {
		t.Fatal("duplicate request")
	}
	if _, e = h.s.CreateRequest(h.ctx, reader, r.ID, "foreign-withdraw", resources.Request{Kind: "withdrawal", Reason: "remove"}); !errors.Is(e, resources.ErrDenied) {
		t.Fatal(e)
	}
}
func TestAmbiguousPublicationRecoveryAndRetryBudget(t *testing.T) {
	h := newFixture(t)
	r := h.draft(t, "recovery", "public")
	r, e := h.s.Submit(h.ctx, uploader, r.ID, "submit", resources.Command{Revision: r.Revision})
	must(t, e)
	h.drain(t)
	r, e = h.s.ApplyDecision(h.ctx, manager, r.ID, "approve", resources.Decision{ID: "approval", Revision: r.Revision, VersionID: r.Proposal.ID, Action: "approve"})
	must(t, e)
	h.files.fail = true
	for n := 0; n < 3; n++ {
		h.clock.Add(time.Minute)
		ok, e := h.s.ProcessOne(h.ctx, "worker")
		if !ok || !errors.Is(e, resources.ErrUnavailable) {
			t.Fatal(ok, e)
		}
	}
	ok, e := h.s.ProcessOne(h.ctx, "worker")
	must(t, e)
	if ok {
		t.Fatal("retry budget ignored")
	}
	_, e = h.s.RetryPending(h.ctx, manager, r.ID, "retry", resources.Command{Revision: r.Revision})
	must(t, e)
	h.files.fail = false
	h.drain(t)
	got, e := h.s.Get(h.ctx, reader, r.ID)
	must(t, e)
	if !got.DownloadAllowed || h.files.writes != 1 {
		t.Fatal("publication retry duplicated remote effect", h.files.writes)
	}
}
func TestDatabaseIsolationAndAtomicAudit(t *testing.T) {
	h := newFixture(t)
	for _, query := range []string{`SELECT * FROM foreign_module.private`, `CREATE TABLE resources.forbidden(id text)`, `UPDATE resources.audit SET data='{}'`, `DELETE FROM resources.replays`, `UPDATE resources.category_revisions SET data='{}'`} {
		if _, e := h.db.Exec(query); e == nil {
			t.Fatalf("runtime escaped privileges: %s", query)
		}
	}
	_, e := h.admin.Exec(`ALTER TABLE resources.outbox ADD CONSTRAINT fixture_reject CHECK(false) NOT VALID`)
	must(t, e)
	if _, e = h.s.CreateDraft(h.ctx, uploader, "rollback", resources.DraftWrite{Metadata: metadata("public")}); e == nil {
		t.Fatal("outbox failure accepted")
	}
	var count int
	must(t, h.admin.QueryRow(`SELECT count(*) FROM resources.records`).Scan(&count))
	if count != 0 {
		t.Fatal("business record survived audit/outbox rollback")
	}
}

func TestTaxonomyChangeDuringPreparationCanBeCorrected(t *testing.T) {
	h := newFixture(t)
	r := h.draft(t, "changed-config", "public")
	r, e := h.s.Submit(h.ctx, uploader, r.ID, "submit", resources.Command{Revision: r.Revision})
	must(t, e)
	h.drain(t)
	r, e = h.s.ApplyDecision(h.ctx, manager, r.ID, "approve", resources.Decision{ID: "approval", Revision: r.Revision, VersionID: r.Proposal.ID, Action: "approve"})
	must(t, e)
	cats, e := h.s.Categories(h.ctx, 25, "", true)
	must(t, e)
	c := cats.Items[0]
	c.Fields[0].Required = true
	_, e = h.s.PutCategory(h.ctx, global, c.ID, "require-year", resources.CategoryWrite{Revision: c.Revision, Names: c.Names, Enabled: true, Fields: c.Fields, GroupBy: c.GroupBy})
	must(t, e)
	_, e = h.s.ProcessOne(h.ctx, "worker")
	if !errors.Is(e, resources.ErrConflict) {
		t.Fatal(e)
	}
	if h.files.writes != 0 {
		t.Fatal("obsolete schema published")
	}
	meta := metadata("public")
	meta.CategoryRevision = 2
	r, e = h.s.UpdateDraft(h.ctx, uploader, r.ID, "reconfirm", resources.DraftWrite{Revision: r.Revision, Metadata: meta})
	must(t, e)
	r = h.publish(t, r)
	if r.Current.Category.Revision != 2 {
		t.Fatal("corrected schema not published")
	}
	meta.Access = "restricted"
	r, e = h.s.UpdateDraft(h.ctx, uploader, r.ID, "access-change", resources.DraftWrite{Revision: r.Revision, Metadata: meta})
	must(t, e)
	if r.Proposal.File != nil {
		t.Fatal("public file reused for restricted protection")
	}
	if _, e = h.s.Submit(h.ctx, uploader, r.ID, "no-matching-file", resources.Command{Revision: r.Revision}); !errors.Is(e, resources.ErrConflict) {
		t.Fatal(e)
	}
}
func TestInitialCategoriesMigrationPreservesOperatorChanges(t *testing.T) {
	h := newFixture(t)
	must(t, rp.Migrate(h.ctx, h.admin))
	cats, e := h.s.Categories(h.ctx, 100, "", true)
	must(t, e)
	if len(cats.Items) != 5 {
		t.Fatal(cats)
	}
	var initial resources.Category
	for _, c := range cats.Items {
		if c.ID == "past-papers" {
			initial = c
		}
	}
	if len(initial.Names) != 8 || initial.GroupBy != "year" {
		t.Fatal("initial configuration missing")
	}
	_, e = h.s.PutCategory(h.ctx, global, initial.ID, "rename-seed", resources.CategoryWrite{Revision: 1, Names: resources.Names{"zh-CN": "课程试题"}, Enabled: false, Fields: initial.Fields, GroupBy: initial.GroupBy})
	must(t, e)
	must(t, rp.Migrate(h.ctx, h.admin))
	cats, e = h.s.Categories(h.ctx, 100, "", true)
	must(t, e)
	for _, c := range cats.Items {
		if c.ID == initial.ID && (c.Revision != 2 || c.Enabled || c.Names["zh-CN"] != "课程试题") {
			t.Fatal("migration overwrote operator configuration")
		}
	}
}

func (m *moderation) Status(context.Context, resources.Principal, string) (resources.RequestStatus, error) {
	return resources.RequestStatus{State: "waiting"}, nil
}

func TestModerationDeliveryReplayAfterPublicationAndStateChanges(t *testing.T) {
	h := newFixture(t)
	draft := h.draft(t, "moderation-replay", "public")
	submitted, e := h.s.Submit(h.ctx, uploader, draft.ID, "submit", resources.Command{Revision: draft.Revision})
	must(t, e)
	decision := resources.Decision{ID: "trusted-decision", VersionID: submitted.Proposal.ID, Revision: submitted.Revision, Action: "publish", Reason: "Reviewed"}
	_, e = h.s.ApplyContentDecision(h.ctx, manager, decision)
	must(t, e)
	h.drain(t)
	result, e := h.s.ApplyContentDecision(h.ctx, manager, decision)
	must(t, e)
	if result.Proposal == nil || result.Proposal.State != "preparing" {
		t.Fatal("lost decision replay", result)
	}
	active, e := h.s.Get(h.ctx, manager, draft.ID)
	must(t, e)
	hidden := resources.Decision{ID: "trusted-hide", VersionID: active.Current.ID, Revision: active.Revision, Action: "hide", Reason: "Safety issue"}
	_, e = h.s.ApplyContentDecision(h.ctx, manager, hidden)
	must(t, e)
	_, e = h.s.ApplyContentDecision(h.ctx, manager, hidden)
	must(t, e)
	active, e = h.s.Get(h.ctx, manager, draft.ID)
	must(t, e)
	if active.State != "blocked" || active.DownloadAllowed {
		t.Fatal("hide did not block", active)
	}
	decision.Reason = "forged replay"
	if _, e = h.s.ApplyContentDecision(h.ctx, manager, decision); !errors.Is(e, resources.ErrConflict) {
		t.Fatal(e)
	}
}
