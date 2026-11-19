package resources_test

import (
	"context"
	"errors"
	r "github.com/StuHelper/stuhelper-resources"
	"sync/atomic"
	"testing"
	"time"
)

type refBinding struct{ versionID, mediaID string }

type hookedFiles struct {
	*files
	svc         **r.Service
	onPublish   func(r.Publication, string)
	asyncOnce   atomic.Bool
	failRelease atomic.Bool
	checkAuth   bool
	refBindings map[string]refBinding
}

func (f *hookedFiles) Publish(ctx context.Context, in r.Publication) (string, bool, error) {
	if f.asyncOnce.CompareAndSwap(true, false) {
		f.mu.Lock()
		ref := "ref-async-" + in.VersionID[:8]
		f.refs[in.OperationID] = ref
		if f.refBindings != nil {
			f.refBindings[ref] = refBinding{in.VersionID, in.MediaVersionID}
		}
		f.writes++
		f.mu.Unlock()
		return ref, false, nil
	}
	ref, ready, err := f.files.Publish(ctx, in)
	if err == nil && ref != "" {
		f.mu.Lock()
		if f.refBindings != nil {
			f.refBindings[ref] = refBinding{in.VersionID, in.MediaVersionID}
		}
		f.mu.Unlock()
	}
	if err == nil && f.onPublish != nil {
		cb := f.onPublish
		f.onPublish = nil
		cb(in, ref)
	}
	return ref, ready, err
}

func (f *hookedFiles) Release(ctx context.Context, ref, key string) error {
	if f.failRelease.Load() {
		return r.ErrUnavailable
	}
	if f.checkAuth && f.svc != nil && *f.svc != nil {
		f.mu.Lock()
		binding, ok := f.refBindings[ref]
		f.mu.Unlock()
		if ok {
			if e := (*f.svc).AuthorizeFile(ctx, manager, r.FileBinding{VersionID: binding.versionID, MediaVersionID: binding.mediaID, ReferenceID: ref}, "release"); e != nil {
				return e
			}
		}
	}
	return f.files.Release(ctx, ref, key)
}

func TestPublishMediaReferenceReleasedOnConcurrentTaxonomyChangeBlockAndFailedReleaseRequeue(t *testing.T) {
	// Case 1: Concurrent category modification after Files.Publish succeeds.
	h := newFixture(t)
	hf := &hookedFiles{files: h.files, svc: &h.s, checkAuth: true, refBindings: map[string]refBinding{}}
	var e error
	h.s, e = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: hf, Moderation: h.moderation}, h.cfg)
	must(t, e)

	item := h.draft(t, "race-cat", "public")
	item, e = h.s.Submit(h.ctx, uploader, item.ID, "sub-race-cat", r.Command{Revision: item.Revision})
	must(t, e)
	h.drain(t)
	item, e = h.s.ApplyDecision(h.ctx, manager, item.ID, "app-race-cat", r.Decision{ID: "dec-race-cat", Revision: item.Revision, VersionID: item.Proposal.ID, Action: "approve"})
	must(t, e)

	var createdRef string
	hf.onPublish = func(_ r.Publication, ref string) {
		createdRef = ref
		cats, err := h.s.Categories(h.ctx, 25, "", true)
		must(t, err)
		c := cats.Items[0]
		_, err = h.s.PutCategory(h.ctx, global, c.ID, "race-cat-bump", r.CategoryWrite{Revision: c.Revision, Names: r.Names{"zh-CN": "并发改名分类"}, Enabled: true, Fields: c.Fields, GroupBy: c.GroupBy})
		must(t, err)
	}
	ran, err := h.s.ProcessOne(h.ctx, "worker")
	if !ran || !errors.Is(err, r.ErrConflict) {
		t.Fatalf("expected conflict on concurrent category change, got ran=%v err=%v", ran, err)
	}
	if createdRef == "" {
		t.Fatal("Files.Publish was not invoked")
	}
	h.drain(t)
	h.files.mu.Lock()
	released1 := h.files.released[createdRef]
	h.files.mu.Unlock()
	if !released1 {
		t.Fatalf("leaked media reference %s after concurrent category conflict", createdRef)
	}
	meta := metadata("public")
	meta.CategoryRevision = 2
	item, e = h.s.UpdateDraft(h.ctx, uploader, item.ID, "recover-race-cat", r.DraftWrite{Revision: item.Revision, Metadata: meta})
	must(t, e)
	item = h.publish(t, item)
	if item.Current == nil || item.Current.Category.Revision != 2 {
		t.Fatal("failed to publish corrected version after category race", item)
	}

	// Case 2: Resource blocked after async Files.Publish (ready=false) persisted v.ReferenceID.
	h2 := newFixture(t)
	hf2 := &hookedFiles{files: h2.files, svc: &h2.s, checkAuth: true, refBindings: map[string]refBinding{}}
	hf2.asyncOnce.Store(true)
	h2.s, e = r.New(h2.repo, r.Dependencies{Authority: h2.auth, Catalog: h2.catalog, Files: hf2, Moderation: h2.moderation}, h2.cfg)
	must(t, e)

	blockedItem := h2.draft(t, "async-block", "public")
	propID := blockedItem.Proposal.ID
	blockedItem, e = h2.s.Submit(h2.ctx, uploader, blockedItem.ID, "sub-block", r.Command{Revision: blockedItem.Revision})
	must(t, e)
	h2.drain(t)
	blockedItem, e = h2.s.ApplyDecision(h2.ctx, manager, blockedItem.ID, "app-block", r.Decision{ID: "dec-block", Revision: blockedItem.Revision, VersionID: propID, Action: "approve"})
	must(t, e)

	// First attempt creates ref-async-... with ready=false and stores ReferenceID on version.
	ran, e = h2.s.ProcessOne(h2.ctx, "worker")
	must(t, e)
	if !ran {
		t.Fatal("expected async publish attempt")
	}
	asyncRef := "ref-async-" + propID[:8]

	// Administrator blocks the resource while media preparation is pending.
	_, e = h2.s.SetState(h2.ctx, manager, blockedItem.ID, "block-preparing", r.StateChange{Revision: blockedItem.Revision, State: "blocked", Reason: "违规封禁"})
	must(t, e)
	h2.clock.Add(time.Minute)
	h2.drain(t)

	h2.files.mu.Lock()
	released2 := h2.files.released[asyncRef]
	h2.files.mu.Unlock()
	if !released2 {
		t.Fatalf("leaked async media reference %s after resource was blocked", asyncRef)
	}

	// Case 3: A previously failed release job is reactivated when queued again.
	h3 := newFixture(t)
	hf3 := &hookedFiles{files: h3.files, svc: &h3.s, refBindings: map[string]refBinding{}}
	hf3.asyncOnce.Store(true)
	h3.s, e = r.New(h3.repo, r.Dependencies{Authority: h3.auth, Catalog: h3.catalog, Files: hf3, Moderation: h3.moderation}, h3.cfg)
	must(t, e)

	requeueItem := h3.draft(t, "requeue-rel", "public")
	requeueProp := requeueItem.Proposal.ID
	requeueItem, e = h3.s.Submit(h3.ctx, uploader, requeueItem.ID, "sub-req", r.Command{Revision: requeueItem.Revision})
	must(t, e)
	h3.drain(t)
	requeueItem, e = h3.s.ApplyDecision(h3.ctx, manager, requeueItem.ID, "app-req", r.Decision{ID: "dec-req", Revision: requeueItem.Revision, VersionID: requeueProp, Action: "approve"})
	must(t, e)
	_, e = h3.s.ProcessOne(h3.ctx, "worker")
	must(t, e)
	requeueRef := "ref-async-" + requeueProp[:8]

	// Superseding draft queues release; make Release fail all MaxAttempts so job enters failed.
	hf3.failRelease.Store(true)
	requeueItem, e = h3.s.UpdateDraft(h3.ctx, uploader, requeueItem.ID, "supersede-1", r.DraftWrite{Revision: requeueItem.Revision, Metadata: metadata("public")})
	must(t, e)
	for n := 0; n < 10; n++ {
		h3.clock.Add(time.Minute)
		ran, _ := h3.s.ProcessOne(h3.ctx, "worker")
		if !ran {
			break
		}
	}
	var failedReleases int
	must(t, h3.admin.QueryRow(`SELECT count(*) FROM resources.jobs WHERE state='failed' AND data->>'Kind'='release'`).Scan(&failedReleases))
	if failedReleases != 1 {
		t.Fatalf("expected 1 failed release job, got %d", failedReleases)
	}

	// The supported administrator retry must recover the release without
	// reconstructing the superseded version in the database.
	adminView, err := h3.s.Get(h3.ctx, manager, requeueItem.ID)
	if err != nil || adminView.OperationState != "failed" {
		t.Fatal("failed release is not visible to the administrator", err, adminView.OperationState)
	}
	hf3.failRelease.Store(false)
	_, e = h3.s.RetryPending(h3.ctx, manager, requeueItem.ID, "retry-release", r.Command{Revision: requeueItem.Revision})
	must(t, e)
	h3.drain(t)
	h3.files.mu.Lock()
	released3 := h3.files.released[requeueRef]
	h3.files.mu.Unlock()
	if !released3 {
		t.Fatalf("failed release job was not reactivated for %s", requeueRef)
	}
}

func TestTemporaryBlockKeepsPendingReviewRecoverable(t *testing.T) {
	h := newFixture(t)
	pub := h.publish(t, h.draft(t, "approved-original", "public"))
	correction, err := h.s.UpdateDraft(h.ctx, uploader, pub.ID, "correct", r.DraftWrite{Revision: pub.Revision, Metadata: metadata("public")})
	must(t, err)
	pending, err := h.s.Submit(h.ctx, uploader, pub.ID, "resubmit", r.Command{Revision: correction.Revision})
	must(t, err)
	blocked, err := h.s.SetState(h.ctx, manager, pub.ID, "temporary-block", r.StateChange{Revision: pending.Revision, State: "blocked", Reason: "临时停用"})
	must(t, err)
	for i := 0; i < h.cfg.MaxAttempts; i++ {
		h.clock.Add(time.Hour)
		_, err = h.s.ProcessOne(h.ctx, "blocked-worker")
		must(t, err)
	}
	_, err = h.s.SetState(h.ctx, manager, pub.ID, "restore", r.StateChange{Revision: blocked.Revision, State: "published", Reason: "恢复"})
	must(t, err)
	// Restoring the resource automatically requeues the waiting review.
	h.drain(t)
	must(t, h.repo.Transaction(h.ctx, func(tx r.Transaction) error {
		v, err := tx.Version(pending.Proposal.ID)
		if err == nil && v.ModerationTaskID == "" {
			t.Fatal("review was not delivered after restoring resource")
		}
		return err
	}))
}

func TestBlockedApprovedProposalRestartsWithFreshOperationAndSourceRetention(t *testing.T) {
	for _, path := range []string{"admin", "moderation"} {
		t.Run(path, func(t *testing.T) {
			h := newFixture(t)
			pub := h.publish(t, h.draft(t, "current", "public"))
			next, err := h.s.UpdateDraft(h.ctx, uploader, pub.ID, "new-version", r.DraftWrite{Revision: pub.Revision, Metadata: metadata("public")})
			must(t, err)
			next, err = h.s.Submit(h.ctx, uploader, pub.ID, "submit-new", r.Command{Revision: next.Revision})
			must(t, err)
			h.drain(t)
			version := next.Proposal.ID
			file := next.Proposal.File.VersionID
			next, err = h.s.ApplyDecision(h.ctx, manager, pub.ID, "approve-new", r.Decision{ID: "approved", Revision: next.Revision, VersionID: version, Action: "approve"})
			must(t, err)
			f := &hookedFiles{files: h.files}
			f.asyncOnce.Store(true)
			h.s, err = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: f, Moderation: h.moderation}, h.cfg)
			must(t, err)
			_, err = h.s.ProcessOne(h.ctx, "first-publish")
			must(t, err)
			readVersion := func() r.VersionRecord {
				t.Helper()
				var v r.VersionRecord
				must(t, h.repo.Transaction(h.ctx, func(tx r.Transaction) error { var e error; v, e = tx.Version(version); return e }))
				return v
			}
			old := readVersion().ReferenceID
			if old == "" {
				t.Fatal("first publication did not attach its reference")
			}
			change := func(in r.Resource, blocked bool) r.Resource {
				t.Helper()
				action, state := "restore", "published"
				if blocked {
					action, state = "hide", "blocked"
				}
				if path == "admin" {
					out, e := h.s.SetState(h.ctx, manager, pub.ID, action, r.StateChange{Revision: in.Revision, State: state, Reason: "policy"})
					must(t, e)
					return out
				}
				out, e := h.s.ApplyContentDecision(h.ctx, manager, r.Decision{ID: action, VersionID: pub.Current.ID, Revision: in.Revision, Action: action, Reason: "policy"})
				must(t, e)
				return out
			}
			blocked := change(next, true)
			keep, err := h.s.RetainsSource(h.ctx, manager, file)
			if err != nil || !keep {
				t.Fatal("blocked proposal lost its source", err)
			}
			h.drain(t)
			if !h.files.released[old] || readVersion().ReferenceID != "" {
				t.Fatal("blocked publication kept the old reference")
			}
			_ = change(blocked, false)
			h.drain(t)
			got, err := h.s.Get(h.ctx, uploader, pub.ID)
			must(t, err)
			active := readVersion()
			if got.Current == nil || got.Current.ID != version || got.Proposal != nil || active.ReferenceID == "" || active.ReferenceID == old || h.files.released[active.ReferenceID] {
				t.Fatal("unblock did not publish a fresh live reference")
			}
			keep, err = h.s.RetainsSource(h.ctx, manager, file)
			if err != nil || keep {
				t.Fatal("completed proposal still holds the source", err)
			}
		})
	}
}

func TestPublishResultArrivingAfterUnblockCannotReplaceNewReference(t *testing.T) {
	h := newFixture(t)
	draft := h.draft(t, "late-publication", "public")
	pending, err := h.s.Submit(h.ctx, uploader, draft.ID, "submit", r.Command{Revision: draft.Revision})
	must(t, err)
	h.drain(t)
	approved, err := h.s.ApplyDecision(h.ctx, manager, draft.ID, "approve", r.Decision{ID: "approved", VersionID: draft.Proposal.ID, Revision: pending.Revision, Action: "approve"})
	must(t, err)
	files := &hookedFiles{files: h.files}
	var late string
	files.onPublish = func(_ r.Publication, ref string) {
		late = ref
		blocked, e := h.s.SetState(h.ctx, manager, draft.ID, "block", r.StateChange{Revision: approved.Revision, State: "blocked", Reason: "manual"})
		must(t, e)
		_, e = h.s.SetState(h.ctx, manager, draft.ID, "restore", r.StateChange{Revision: blocked.Revision, State: "published", Reason: "restore"})
		must(t, e)
	}
	h.s, err = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: files, Moderation: h.moderation}, h.cfg)
	must(t, err)
	h.drain(t)
	must(t, h.repo.Transaction(h.ctx, func(tx r.Transaction) error {
		v, e := tx.Version(draft.Proposal.ID)
		if e != nil {
			return e
		}
		if late == "" || !h.files.released[late] || v.Version.State != "published" || v.ReferenceID == late || h.files.released[v.ReferenceID] {
			t.Fatal("late publish result escaped retirement", v.Version.State)
		}
		return nil
	}))
}

func TestWithdrawnOrDeletedProposalStopsRetainingSource(t *testing.T) {
	for _, action := range []string{"withdraw", "delete"} {
		t.Run(action, func(t *testing.T) {
			h := newFixture(t)
			pub := h.publish(t, h.draft(t, "current", "public"))
			correction, err := h.s.UpdateDraft(h.ctx, uploader, pub.ID, "new", r.DraftWrite{Revision: pub.Revision, Metadata: metadata("public")})
			must(t, err)
			pending, err := h.s.Submit(h.ctx, uploader, pub.ID, "submit-new", r.Command{Revision: correction.Revision})
			must(t, err)
			blocked, err := h.s.SetState(h.ctx, manager, pub.ID, "block", r.StateChange{Revision: pending.Revision, State: "blocked", Reason: "hold"})
			must(t, err)
			file := pending.Proposal.File.VersionID
			keep, err := h.s.RetainsSource(h.ctx, manager, file)
			if err != nil || !keep {
				t.Fatal("missing hold on pending proposal", err)
			}
			if action == "delete" {
				_, err = h.s.ApplyContentDecision(h.ctx, manager, r.Decision{ID: "delete", VersionID: pub.Current.ID, Revision: blocked.Revision, Action: "delete", Reason: "removed"})
			} else {
				restored, e := h.s.SetState(h.ctx, manager, pub.ID, "restore", r.StateChange{Revision: blocked.Revision, State: "published", Reason: "restore"})
				must(t, e)
				_, err = h.s.WithdrawProposal(h.ctx, uploader, pub.ID, "withdraw", r.Command{Revision: restored.Revision})
			}
			must(t, err)
			keep, err = h.s.RetainsSource(h.ctx, manager, file)
			if err != nil || keep {
				t.Fatal("obsolete version still holds the source", err)
			}
		})
	}
}
