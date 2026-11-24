package resources_test

import (
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

func TestClosedOwnerExpiresOnlyAfterRecordedCompletion(t *testing.T) {
	f := newFixture(t)
	runtime, ownerDB := f.repo, f.admin
	ctx := t.Context()
	var role string
	if err := runtime.DB.QueryRowContext(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerDB.ExecContext(ctx, "GRANT EXECUTE ON FUNCTION resources.mark_closure_completed(text,text,timestamptz),resources.prune_closure_markers(integer) TO "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"expired", "current", "incomplete"} {
		if _, err := ownerDB.ExecContext(ctx, "INSERT INTO resources.closed_owners(owner_key,closure_id) VALUES($1,$2)", account, "closure-"+account); err != nil {
			t.Fatal(err)
		}
	}
	finished := time.Now().UTC().AddDate(-1, 0, -1).Truncate(time.Microsecond)
	if err := runtime.RecordClosureCompletion(ctx, "expired", "wrong", finished); err == nil {
		t.Fatal("accepted another closure")
	}
	for range 2 {
		if err := runtime.RecordClosureCompletion(ctx, "expired", "closure-expired", finished); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.RecordClosureCompletion(ctx, "expired", "closure-expired", finished.Add(time.Hour)); err == nil {
		t.Fatal("changed completion time")
	}
	if err := runtime.RecordClosureCompletion(ctx, "current", "closure-current", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{1, 0} {
		n, err := runtime.PruneClosureMarkers(ctx, 1)
		if err != nil || n != want {
			t.Fatalf("bounded purge: %d %v", n, err)
		}
	}
	var retained int
	if err := ownerDB.QueryRowContext(ctx, "SELECT count(*) FROM resources.closed_owners WHERE owner_key IN ('current','incomplete')").Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("current markers: %d %v", retained, err)
	}
}

func TestTransferredComplaintPurgeIsScopedAndRepeatable(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var role string
	if err := f.repo.DB.QueryRow("SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	_, err := f.admin.Exec("GRANT EXECUTE ON FUNCTION resources.purge_transferred_complaint(text) TO " + pgx.Identifier{role}.Sanitize())
	must(t, err)
	_, err = f.admin.Exec(`INSERT INTO resources.records(id,owner_key,school_id,state,active_id,proposal_id,data)
 VALUES('resource','fixture','school','draft','','','{}')`)
	must(t, err)
	for _, id := range []string{"closed", "held"} {
		_, err = f.admin.Exec(`INSERT INTO resources.requests(id,resource_id,data) VALUES($1,'resource',
   jsonb_build_object('ExternalID',$1::text,'Request',jsonb_build_object('kind','complaint','reason','synthetic'),
   'Receipt',jsonb_build_object('id',$1::text,'state','submitted','created_at','2024-01-01T00:00:00Z')))`, id)
		must(t, err)
		_, err = f.admin.Exec(`INSERT INTO resources.replays(key,data) SELECT $1,jsonb_build_object('Result',data->'Receipt') FROM resources.requests WHERE id=$1`, id)
		must(t, err)
	}
	for range 2 {
		must(t, f.repo.PurgeTransferredComplaint(ctx, "closed"))
	}
	var c int
	err = f.admin.QueryRow(`SELECT (SELECT count(*) FROM resources.requests WHERE id='held')+(SELECT count(*) FROM resources.replays WHERE key='held')-
 (SELECT count(*) FROM resources.requests WHERE id='closed')-(SELECT count(*) FROM resources.replays WHERE key='closed')`).Scan(&c)
	if err != nil || c != 2 {
		t.Fatal(c, err)
	}
}
