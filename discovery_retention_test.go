package resources_test

import (
	"github.com/jackc/pgx/v5"
	"testing"
)

func TestDiscoveryRetentionRequiresSupersessionDeliveryAndNoHold(t *testing.T) {
	f := newFixture(t)
	repo, owner := f.repo, f.admin
	ctx := t.Context()
	var role string
	if err := repo.DB.QueryRow("SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	run := func(query string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	run("GRANT EXECUTE ON FUNCTION resources.maintain_discovery_history(integer),resources.claim_discovery_retirements(integer),resources.seal_discovery_retirement(text,bigint),resources.ack_discovery_retirement(text,bigint) TO " + pgx.Identifier{role}.Sanitize())
	run(`INSERT INTO resources.discovery_history(id,revision,data,retention_at) VALUES
 ('expired',1,'{"visible":true}',now()-interval '95 days'),('expired',2,'{"visible":false}',now()-interval '91 days'),
 ('held',1,'{"visible":false}',now()-interval '91 days'),('unconfirmed',1,'{"visible":false}',now()-interval '91 days'),
 ('recent',1,'{"visible":true}',now()-interval '95 days'),('recent',2,'{"visible":true}',now()-interval '89 days')`)
	for _, id := range []string{"expired", "held"} {
		rev := 1
		if id == "expired" {
			rev = 2
		}
		run(`INSERT INTO resources.outbox(id,data) VALUES($1,jsonb_build_object('type','resources.discovery.v1','subject',$1::text,'data',jsonb_build_object('revision',$2::bigint,'visible',false)))`, id, rev)
		run("UPDATE resources.outbox SET sent_at=now() WHERE id=$1", id)
	}
	run(`INSERT INTO resources.discovery_holds(id,revision,case_ref,basis,purpose,end_condition,review_at,until_at)
 VALUES('held',1,'case','review','verify','resolution',now(),now()+interval '1 day')`)
	candidates, _, err := repo.MaintainDiscoveryHistory(ctx, 100)
	if err != nil || candidates["expired"] != 2 {
		t.Fatal("delivery proof missing", err)
	}
	check := func(id string, rev int64, want bool) {
		t.Helper()
		ok, err := repo.SealDiscoveryRetirement(ctx, id, rev)
		if err != nil || ok != want {
			t.Fatalf("seal %s: %v %v", id, ok, err)
		}
	}
	check("held", 1, false)
	check("unconfirmed", 1, false)
	run(`INSERT INTO resources.outbox(id,data) VALUES('pending','{"type":"resources.discovery.v1","subject":"expired","data":{"revision":1,"visible":true}}')`)
	check("expired", 2, false)
	run("UPDATE resources.outbox SET sent_at=now() WHERE id='pending'")
	check("expired", 2, true)
	check("expired", 2, true)
	if err := repo.AcknowledgeDiscoveryRetirement(ctx, "expired", 2); err != nil {
		t.Fatal(err)
	}
	var old, recent, other int
	if err := owner.QueryRow(`SELECT (SELECT count(*) FROM resources.discovery_history WHERE id='expired'),(SELECT count(*) FROM resources.discovery_history WHERE id='recent'),
 (SELECT count(*) FROM resources.discovery_history WHERE id IN('unconfirmed','held'))`).Scan(&old, &recent, &other); err != nil || old != 0 || recent != 2 || other != 2 {
		t.Fatal(old, recent, other, err)
	}
}
