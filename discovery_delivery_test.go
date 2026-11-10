package resources_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	m "github.com/StuHelper/stuhelper-resources"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDiscoveryOutboxNotificationRollbackExclusionAndConfirmation(t *testing.T) {
	f := newFixture(t)
	repo, db := f.repo, f.admin
	ctx := context.Background()
	initial, e := repo.PendingOutbox(ctx, 100, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, event := range initial {
		if e = repo.ConfirmOutbox(ctx, event.ID); e != nil {
			t.Fatal(e)
		}
	}
	var beforeAudits int
	if e = db.QueryRow(`SELECT count(*) FROM resources.audit`).Scan(&beforeAudits); e != nil {
		t.Fatal(e)
	}
	listener, e := repo.DB.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	if _, e = listener.ExecContext(ctx, "LISTEN "+m.OutboxChannel); e != nil {
		t.Fatal(e)
	}
	publish := func(tx m.Transaction, id, subject string, revision int64) error {
		return tx.Event(m.Event{ID: id, Type: "resources.test.v1", ResourceID: subject, Revision: revision, Time: time.Now()})
	}
	rollback := errors.New("intentional rollback")
	e = repo.Transaction(ctx, func(tx m.Transaction) error {
		if e := publish(tx, "rollback", "object", 1); e != nil {
			return e
		}
		pending, e := repo.PendingOutbox(ctx, 100, nil)
		if e != nil || len(pending) != 0 {
			t.Fatal("uncommitted event visible", pending, e)
		}
		wait, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
		defer cancel()
		e = listener.Raw(func(raw any) error { _, e := raw.(*stdlib.Conn).Conn().WaitForNotification(wait); return e })
		if !errors.Is(e, context.DeadlineExceeded) {
			t.Fatal("notified before commit", e)
		}
		return rollback
	})
	if !errors.Is(e, rollback) {
		t.Fatal(e)
	}
	noNotice, cancelNoNotice := context.WithTimeout(ctx, 40*time.Millisecond)
	e = listener.Raw(func(raw any) error { _, e := raw.(*stdlib.Conn).Conn().WaitForNotification(noNotice); return e })
	cancelNoNotice()
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("rollback notified", e)
	}
	pending, e := repo.PendingOutbox(ctx, 100, nil)
	if e != nil || len(pending) != 0 {
		t.Fatal("rollback outbox", pending, e)
	}
	var audits int
	if e = db.QueryRow(`SELECT count(*) FROM resources.audit`).Scan(&audits); e != nil || audits != beforeAudits {
		t.Fatal("rollback audit", audits, e)
	}
	e = repo.Transaction(ctx, func(tx m.Transaction) error {
		for _, v := range []struct {
			id, subject string
			rev         int64
		}{{"z-first", "a-blocked", 1}, {"a-second", "a-blocked", 2}, {"ready", "b-ready", 1}} {
			if e := publish(tx, v.id, v.subject, v.rev); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	wait, cancel := context.WithTimeout(ctx, time.Second)
	e = listener.Raw(func(raw any) error {
		n, e := raw.(*stdlib.Conn).Conn().WaitForNotification(wait)
		if e == nil && (n.Channel != m.OutboxChannel || n.Payload != "") {
			t.Error(n)
		}
		return e
	})
	cancel()
	if e != nil {
		t.Fatal(e)
	}
	pending, e = repo.PendingOutbox(ctx, 100, nil)
	if e != nil || len(pending) != 3 || pending[0].ID != "z-first" || pending[1].ID != "a-second" {
		t.Fatal(pending, e)
	}
	again, e := repo.PendingOutbox(ctx, 100, []string{})
	if e != nil || !reflect.DeepEqual(pending, again) {
		t.Fatal("reading claimed events", e)
	}
	ready, e := repo.PendingOutbox(ctx, 1, []string{"a-blocked", "a-blocked", "unknown"})
	if e != nil || len(ready) != 1 || ready[0].ID != "ready" {
		t.Fatal(ready, e)
	}
	for _, v := range pending {
		if e = repo.ConfirmOutbox(ctx, v.ID); e != nil {
			t.Fatal(e)
		}
	}
	var first, second time.Time
	if e = db.QueryRow(`SELECT sent_at FROM resources.outbox WHERE id='ready'`).Scan(&first); e != nil {
		t.Fatal(e)
	}
	if e = repo.ConfirmOutbox(ctx, "ready"); e != nil {
		t.Fatal(e)
	}
	if e = db.QueryRow(`SELECT sent_at FROM resources.outbox WHERE id='ready'`).Scan(&second); e != nil || !first.Equal(second) {
		t.Fatal(first, second, e)
	}
	if e = repo.ConfirmOutbox(ctx, "missing"); !errors.Is(e, m.ErrNotFound) {
		t.Fatal(e)
	}
	if _, e = listener.ExecContext(ctx, "UNLISTEN *"); e != nil {
		t.Fatal(e)
	}
	if e = repo.Transaction(ctx, func(tx m.Transaction) error {
		for i := range 105 {
			if e := publish(tx, fmt.Sprintf("backlog-%03d", i), "backlog", int64(i+1)); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	pending, e = repo.PendingOutbox(ctx, 100, nil)
	if e != nil || len(pending) != 100 {
		t.Fatal(len(pending), e)
	}
	for _, v := range pending {
		if e = repo.ConfirmOutbox(ctx, v.ID); e != nil {
			t.Fatal(e)
		}
	}
	pending, e = repo.PendingOutbox(ctx, 100, nil)
	if e != nil || len(pending) != 5 {
		t.Fatal(len(pending), e)
	}
	for _, limit := range []int{0, 101} {
		if _, e = repo.PendingOutbox(ctx, limit, nil); !errors.Is(e, m.ErrInput) {
			t.Fatal(e)
		}
	}
	if _, e = repo.PendingOutbox(ctx, 1, make([]string, 1001)); !errors.Is(e, m.ErrInput) {
		t.Fatal(e)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, e = repo.PendingOutbox(cancelled, 1, nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestDiscoverySnapshotStableVersionsTombstonesAndSchema(t *testing.T) {
	f := newFixture(t)
	repo, db := f.repo, f.admin
	ctx := context.Background()
	save := func(tx m.Transaction, id, state string, revision int64) error {
		r, e := tx.Record(id)
		if errors.Is(e, m.ErrNotFound) {
			r = m.Record{ID: id, Owner: m.Principal{Issuer: "private-issuer", Subject: "private-owner"}, SchoolID: "school", Revision: 1, State: "draft", CreatedAt: time.Now()}
			if e = tx.SaveRecord(r); e != nil {
				return e
			}
			v := m.VersionRecord{ResourceID: id, ReferenceID: "private-reference", Version: m.Version{ID: "version-" + id, Number: 1, State: "published", CreatedAt: time.Now(), Metadata: m.Metadata{Title: "Public title", Description: "private-body", Origin: m.Origin{Author: "private-author"}, Associations: m.Associations{SchoolID: "school", CourseID: "course", TeacherID: "teacher-a"}}}}
			if e = tx.SaveVersion(v); e != nil {
				return e
			}
			r.ActiveID = v.Version.ID
		} else if e != nil {
			return e
		}
		r.State = state
		r.Revision = revision
		return tx.SaveRecord(r)
	}
	if e := repo.Transaction(ctx, func(tx m.Transaction) error {
		for _, id := range []string{"a", "b", "c"} {
			if e := save(tx, id, "published", 2); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- repo.Transaction(ctx, func(tx m.Transaction) error {
			if e := save(tx, "z-late", "published", 2); e != nil {
				return e
			}
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case e := <-done:
		t.Fatal(e)
	}
	page, e := repo.DiscoverySnapshot(ctx, "", 1)
	if e != nil || len(page.Items) != 1 || page.Next == "" {
		close(release)
		t.Fatal(page, e)
	}
	cursor := page.Next
	seen := []m.DiscoveryDocument{page.Items[0]}
	close(release)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if e = repo.Transaction(ctx, func(tx m.Transaction) error { return save(tx, "c", "unlisted", 2+1) }); e != nil {
		t.Fatal(e)
	}
	for page.Next != "" {
		old := page.Next
		page, e = repo.DiscoverySnapshot(ctx, page.Next, 1)
		if e != nil || page.Next == old {
			t.Fatal(page, e)
		}
		seen = append(seen, page.Items...)
	}
	if len(seen) != 3 || seen[2].Target.ID != "c" || !seen[2].Visible || seen[2].Revision != 2 {
		t.Fatal(seen)
	}
	fresh, e := repo.DiscoverySnapshot(ctx, "", 100)
	if e != nil || len(fresh.Items) != 4 {
		t.Fatal(fresh, e)
	}
	if fresh.Items[2].Visible || fresh.Items[2].Title != "" || fresh.Items[2].CourseID != "" || len(fresh.Items[2].TeacherIDs) != 0 || fresh.Items[2].Revision != 2+1 {
		t.Fatal(fresh.Items[2])
	}
	// The checked-in schema is the search 9e791c6 source contract, not a struct mirror.
	raw, e := os.ReadFile("contracts/discovery-event.schema.json")
	if e != nil {
		t.Fatal(e)
	}
	var definition any
	if e = json.Unmarshal(raw, &definition); e != nil {
		t.Fatal(e)
	}
	compiler := jsonschema.NewCompiler()
	if e = compiler.AddResource("urn:discovery", definition); e != nil {
		t.Fatal(e)
	}
	schema, e := compiler.Compile("urn:discovery")
	if e != nil {
		t.Fatal(e)
	}
	events, e := repo.PendingOutbox(ctx, 100, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, event := range events {
		if event.Type != "resources.discovery.v1" {
			continue
		}
		payload, e := json.Marshal(event)
		if e != nil {
			t.Fatal(e)
		}
		var value any
		if e = json.Unmarshal(payload, &value); e != nil {
			t.Fatal(e)
		}
		if e = schema.Validate(value); e != nil {
			t.Fatal(e, string(payload))
		}
		if strings.Contains(string(payload), "private") || strings.Contains(string(payload), "ratings") || strings.Contains(string(payload), "owner") {
			t.Fatal("private data in discovery", string(payload))
		}
		var stored []byte
		if e = db.QueryRow(`SELECT data FROM resources.discovery_history WHERE id=$1 AND revision=($2::jsonb->>'revision')::bigint`, event.Subject, []byte(event.Data)).Scan(&stored); e != nil {
			t.Fatal(e)
		}
		var document, history m.DiscoveryDocument
		if e = json.Unmarshal(event.Data, &document); e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(stored, &history); e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(document, history) || document.Target.ID != event.Subject || document.Target.Source != event.Source {
			t.Fatal(document, history)
		}
	}
	// An outbox failure rolls back the object and its immutable discovery history.
	if _, e = db.Exec(`ALTER TABLE resources.outbox ADD CONSTRAINT reject_discovery CHECK(data->>'type'<>'resources.discovery.v1') NOT VALID`); e != nil {
		t.Fatal(e)
	}
	if e = repo.Transaction(ctx, func(tx m.Transaction) error { return save(tx, "rollback", "published", 2) }); e == nil {
		t.Fatal("outbox failure ignored")
	}
	var count int
	if e = db.QueryRow(`SELECT count(*) FROM resources.discovery_history WHERE id='rollback'`).Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
	if e = db.QueryRow(`SELECT count(*) FROM resources.records WHERE id='rollback'`).Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
	if _, e = db.Exec(`DELETE FROM resources.discovery_history WHERE id='c' AND revision=2`); e != nil {
		t.Fatal(e)
	}
	if _, e = repo.DiscoverySnapshot(ctx, cursor, 1); !errors.Is(e, m.ErrUnavailable) {
		t.Fatal("history loss accepted", e)
	}
	rawCursor, e := base64.RawURLEncoding.DecodeString(cursor)
	if e != nil {
		t.Fatal(e)
	}
	var expired map[string]any
	if e = json.Unmarshal(rawCursor, &expired); e != nil {
		t.Fatal(e)
	}
	expired["Expires"] = time.Now().Add(-time.Minute)
	expiredCursor, e := json.Marshal(expired)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = repo.DiscoverySnapshot(ctx, base64.RawURLEncoding.EncodeToString(expiredCursor), 1); e == nil {
		t.Fatal("expired snapshot restarted")
	}
	for _, limit := range []int{0, 101} {
		if _, e = repo.DiscoverySnapshot(ctx, "", limit); !errors.Is(e, m.ErrInput) {
			t.Fatal(e)
		}
	}
	if _, e = repo.DiscoverySnapshot(ctx, "malformed", 1); e == nil {
		t.Fatal("invalid cursor accepted")
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, e = repo.DiscoverySnapshot(cancelled, "", 1); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
