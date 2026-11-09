package resources_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	r "github.com/StuHelper/stuhelper-resources"
	"strings"
	"testing"
)

type discoveryAuthority struct {
	check func(context.Context, r.Principal, string, string) error
}

func (a discoveryAuthority) Check(ctx context.Context, p r.Principal, action, school string) error {
	return a.check(ctx, p, action, school)
}

func TestDiscoveryPublicationProposalUnlistingDeletionAndPermissions(t *testing.T) {
	h := newFixture(t)
	draft := h.draft(t, "discovery-draft", "public")
	docs, e := h.s.DiscoveryCurrent(h.ctx, reader, []string{draft.ID})
	must(t, e)
	if len(docs) != 0 {
		t.Fatal(docs)
	}
	page, e := h.repo.DiscoverySnapshot(h.ctx, "", 100)
	must(t, e)
	if len(page.Items) != 1 || page.Items[0].Title != "" || page.Items[0].Visible {
		t.Fatal(page)
	}
	item := h.publish(t, draft)
	docs, e = h.s.DiscoveryCurrent(h.ctx, reader, []string{item.ID, "unknown"})
	must(t, e)
	if len(docs) != 1 || docs[0].Revision != item.Revision || docs[0].Title != item.Current.Metadata.Title {
		t.Fatal(docs, item)
	}
	originalTitle := docs[0].Title
	h.auth.set(reader.Key()+":discover", true)
	docs, e = h.s.DiscoveryCurrent(h.ctx, reader, []string{item.ID})
	must(t, e)
	if len(docs) != 0 {
		t.Fatal(docs)
	}
	h.auth.set(reader.Key()+":discover", false)
	if _, e = h.s.DiscoveryCurrent(h.ctx, r.Principal{}, []string{item.ID}); !errors.Is(e, r.ErrUnauthenticated) {
		t.Fatal(e)
	}
	deps := r.Dependencies{Authority: discoveryAuthority{func(context.Context, r.Principal, string, string) error { return r.ErrUnavailable }}, Catalog: h.catalog, Files: h.files, Moderation: h.moderation, Entitlements: h.rights}
	unavailable, e := r.New(h.repo, deps, h.cfg)
	must(t, e)
	if _, e = unavailable.DiscoveryCurrent(h.ctx, reader, []string{item.ID}); !errors.Is(e, r.ErrUnavailable) {
		t.Fatal(e)
	}
	proposed := metadata("public")
	proposed.Title = "PRIVATE proposed title"
	proposed.Description = "PRIVATE body"
	item, e = h.s.UpdateDraft(h.ctx, uploader, item.ID, "proposal", r.DraftWrite{Revision: item.Revision, Metadata: proposed})
	must(t, e)
	docs, e = h.s.DiscoveryCurrent(h.ctx, reader, []string{item.ID})
	must(t, e)
	if len(docs) != 1 || docs[0].Title != originalTitle || docs[0].Revision != item.Revision {
		t.Fatal(docs)
	}
	events, e := h.repo.PendingOutbox(h.ctx, 100, nil)
	must(t, e)
	for _, event := range events {
		if event.Type == "resources.discovery.v1" && strings.Contains(string(event.Data), "PRIVATE") {
			t.Fatal(event)
		}
	}
	for _, state := range []string{"unlisted", "published", "blocked", "published"} {
		item, e = h.s.SetState(h.ctx, manager, item.ID, fmt.Sprintf("state-%s-%d", state, item.Revision), r.StateChange{Revision: item.Revision, State: state, Reason: "reviewed"})
		must(t, e)
		docs, e = h.s.DiscoveryCurrent(h.ctx, reader, []string{item.ID})
		must(t, e)
		if (len(docs) == 1) != (state == "published") {
			t.Fatal(state, docs)
		}
	}
	// The existing trusted delete operation unlists the active publication.
	item, e = h.s.ApplyContentDecision(h.ctx, manager, r.Decision{ID: "discovery-delete", VersionID: item.Current.ID, Revision: item.Revision, Action: "delete", Reason: "reviewed"})
	must(t, e)
	docs, e = h.s.DiscoveryCurrent(h.ctx, reader, []string{item.ID})
	must(t, e)
	if len(docs) != 0 {
		t.Fatal(docs)
	}
	page, e = h.repo.DiscoverySnapshot(h.ctx, "", 100)
	must(t, e)
	if len(page.Items) != 1 || page.Items[0].Visible || page.Items[0].Title != "" || page.Items[0].Revision != item.Revision {
		t.Fatal(page)
	}
	events, e = h.repo.PendingOutbox(h.ctx, 100, nil)
	must(t, e)
	var previous int64
	for _, event := range events {
		if event.Type != "resources.discovery.v1" || event.Subject != item.ID {
			continue
		}
		var d r.DiscoveryDocument
		must(t, json.Unmarshal(event.Data, &d))
		if d.Revision <= previous {
			t.Fatal("subject revisions out of order", previous, d.Revision)
		}
		previous = d.Revision
	}
}

func TestDiscoveryRejectsOtherSchoolAndDoesNotElevateOwners(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "school-source", "public"))
	must(t, h.repo.Transaction(h.ctx, func(tx r.Transaction) error {
		original, e := tx.Record(item.ID)
		if e != nil {
			return e
		}
		v, e := tx.Version(original.ActiveID)
		if e != nil {
			return e
		}
		original.ID = "other-school"
		original.SchoolID = "elsewhere"
		original.Revision = 1
		original.State = "draft"
		original.ActiveID = ""
		original.ProposalID = ""
		if e = tx.SaveRecord(original); e != nil {
			return e
		}
		v.ResourceID = original.ID
		v.Version.ID = "other-version"
		v.Version.Metadata.Associations.SchoolID = "elsewhere"
		if e = tx.SaveVersion(v); e != nil {
			return e
		}
		original.ActiveID = v.Version.ID
		original.State = "published"
		original.Revision = 2
		return tx.SaveRecord(original)
	}))
	for _, principal := range []r.Principal{reader, uploader, manager} {
		docs, e := h.s.DiscoveryCurrent(h.ctx, principal, []string{item.ID, "other-school"})
		must(t, e)
		if len(docs) != 1 || docs[0].Target.ID != item.ID {
			t.Fatal(principal, docs)
		}
	}
}

func TestDiscoveryRechecksPublicationAfterExternalAuthorization(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "current-race", "public"))
	deps := r.Dependencies{Catalog: h.catalog, Files: h.files, Moderation: h.moderation, Entitlements: h.rights}
	deps.Authority = discoveryAuthority{func(ctx context.Context, p r.Principal, action, school string) error {
		_, e := h.s.SetState(ctx, manager, item.ID, "unlist-during-auth", r.StateChange{Revision: item.Revision, State: "unlisted", Reason: "reviewed"})
		return e
	}}
	source, e := r.New(h.repo, deps, h.cfg)
	must(t, e)
	docs, e := source.DiscoveryCurrent(h.ctx, reader, []string{item.ID})
	must(t, e)
	if len(docs) != 0 {
		t.Fatal("stale publication returned", docs)
	}
}
