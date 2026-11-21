CREATE SCHEMA IF NOT EXISTS resources;
CREATE TABLE IF NOT EXISTS resources.categories (id text PRIMARY KEY, revision bigint NOT NULL, enabled boolean NOT NULL, data jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS resources.category_revisions (id text NOT NULL, revision bigint NOT NULL, data jsonb NOT NULL, PRIMARY KEY(id,revision));
CREATE TABLE IF NOT EXISTS resources.records (id text PRIMARY KEY, owner_key text NOT NULL, school_id text NOT NULL, state text NOT NULL, active_id text NOT NULL, proposal_id text NOT NULL, data jsonb NOT NULL, query_changed_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS resources_records_owner ON resources.records(owner_key,id);
CREATE INDEX IF NOT EXISTS resources_records_school ON resources.records(school_id,state,id);
CREATE TABLE IF NOT EXISTS resources.versions (id text PRIMARY KEY, resource_id text NOT NULL REFERENCES resources.records(id), number bigint NOT NULL, data jsonb NOT NULL, UNIQUE(resource_id,number));
CREATE INDEX IF NOT EXISTS resources_proposal_media ON resources.versions ((data->'Version'->'file'->>'version_id')) WHERE data->'Version'->>'state' IN ('pending','preparing');
CREATE INDEX IF NOT EXISTS resources_versions_published_sha256 ON resources.versions((data->'Version'->'metadata'->'associations'->>'school_id'),(data->'Version'->'file'->>'sha256')) WHERE data->'Version'->>'state'='published';
CREATE INDEX IF NOT EXISTS resources_versions_course ON resources.versions ((data->'Version'->'metadata'->'associations'->>'course_id'));
CREATE INDEX IF NOT EXISTS resources_versions_category ON resources.versions ((data->'Version'->'metadata'->>'category_id'));
CREATE INDEX IF NOT EXISTS resources_versions_directory ON resources.versions ((data->'Version'->'metadata'->>'directory_id'));
CREATE INDEX IF NOT EXISTS resources_versions_attributes ON resources.versions USING gin ((COALESCE(data->'Version'->'metadata'->'attributes','{}'::jsonb)) jsonb_path_ops);
CREATE TABLE IF NOT EXISTS resources.grants (id text PRIMARY KEY, resource_id text NOT NULL REFERENCES resources.records(id), subject_key text NOT NULL, data jsonb NOT NULL, query_changed_at timestamptz NOT NULL DEFAULT now(), UNIQUE(resource_id,subject_key));
CREATE INDEX IF NOT EXISTS resources_grants_subject ON resources.grants(subject_key,resource_id);
CREATE TABLE IF NOT EXISTS resources.directories (id text PRIMARY KEY, school_id text NOT NULL, course_id text NOT NULL, parent_id text NOT NULL, data jsonb NOT NULL);
CREATE INDEX IF NOT EXISTS resources_directories_parent ON resources.directories(school_id,parent_id,id);
CREATE INDEX IF NOT EXISTS resources_directories_course ON resources.directories(school_id,course_id,id);
CREATE TABLE IF NOT EXISTS resources.packages (id text PRIMARY KEY, expires_at timestamptz NOT NULL, data jsonb NOT NULL);
CREATE INDEX IF NOT EXISTS resources_packages_expiry ON resources.packages(expires_at);
CREATE TABLE IF NOT EXISTS resources.requests (id text PRIMARY KEY, resource_id text NOT NULL REFERENCES resources.records(id), data jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS resources.jobs (id text PRIMARY KEY, state text NOT NULL, available_at timestamptz NOT NULL, lease_until timestamptz NOT NULL, data jsonb NOT NULL);
ALTER TABLE resources.jobs ADD COLUMN IF NOT EXISTS completed_at timestamptz;
UPDATE resources.jobs SET completed_at=transaction_timestamp() WHERE state='done' AND completed_at IS NULL;
CREATE INDEX IF NOT EXISTS resources_jobs_completed ON resources.jobs(completed_at,id) WHERE state='done';
DROP INDEX IF EXISTS resources.resources_jobs_failed_release;
CREATE INDEX IF NOT EXISTS resources_jobs_failed_cleanup ON resources.jobs((data->>'ResourceID'),available_at,id) WHERE state='failed' AND data->>'Kind' IN ('release','retire_publication');
CREATE INDEX IF NOT EXISTS resources_jobs_claim ON resources.jobs(state,available_at,lease_until);
CREATE INDEX IF NOT EXISTS resources_jobs_resource ON resources.jobs((data->>'ResourceID'),state);
CREATE TABLE IF NOT EXISTS resources.replays (key text PRIMARY KEY, data jsonb NOT NULL);

-- Historical complaints did not record a specific violation category.
UPDATE resources.requests
SET data=jsonb_set(data,'{Request,category}',to_jsonb('other'::text))
WHERE data#>>'{Request,kind}'='complaint'
  AND COALESCE(data#>>'{Request,category}','') NOT IN (
    'illegal_content','pornography','gambling_drugs','fraud','violence_threat','minors_harm',
    'personal_attack','harassment_discrimination','defamation_rumor','privacy_leak','impersonation',
    'copyright','other_ip','academic_integrity','leaked_exam','fake_review','wrong_teacher',
    'wrong_info','spam','malware','duplicate','low_quality','author_update','other'
  );
UPDATE resources.requests
SET data=jsonb_set(data,'{Receipt,category}',data#>'{Request,category}')
WHERE data#>>'{Request,kind}'='complaint'
  AND data#>>'{Receipt,category}' IS DISTINCT FROM data#>>'{Request,category}';
UPDATE resources.replays AS replay
SET data=jsonb_set(replay.data,'{Result,category}',request.data#>'{Request,category}')
FROM resources.requests AS request
WHERE request.id=replay.data#>>'{Result,id}'
  AND replay.data#>>'{Result,kind}'='complaint'
  AND request.data#>>'{Request,kind}'='complaint'
  AND replay.data#>>'{Result,category}' IS DISTINCT FROM request.data#>>'{Request,category}';

CREATE TABLE IF NOT EXISTS resources.audit (id text PRIMARY KEY, data jsonb NOT NULL, reason text NOT NULL DEFAULT '', result text NOT NULL DEFAULT 'unknown' CHECK(result IN ('success','denied','failed','unknown')), recorded_xid xid8 NOT NULL DEFAULT pg_current_xact_id(), audit_actor text NOT NULL DEFAULT 'unknown', audit_target text NOT NULL DEFAULT 'unknown', audit_action text NOT NULL DEFAULT 'unknown', audit_school text NOT NULL DEFAULT '*', audit_group text NOT NULL DEFAULT '*', audit_time timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS resources.outbox(id text PRIMARY KEY, data jsonb NOT NULL, seq bigint GENERATED ALWAYS AS IDENTITY UNIQUE, sent_at timestamptz);
DROP INDEX IF EXISTS resources.resources_outbox_pending;
CREATE INDEX IF NOT EXISTS resources_outbox_pending_seq ON resources.outbox(seq) WHERE sent_at IS NULL;
CREATE INDEX IF NOT EXISTS resources_outbox_sent ON resources.outbox(sent_at) WHERE sent_at IS NOT NULL;
REVOKE ALL ON SCHEMA resources FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA resources FROM PUBLIC;

CREATE INDEX IF NOT EXISTS resources_requests_reporter_c ON resources.requests((data->'Reporter'),id COLLATE "C");
DROP INDEX IF EXISTS resources.resources_requests_reporter;

CREATE TABLE IF NOT EXISTS resources.closed_owners(owner_key text PRIMARY KEY,closure_id text NOT NULL,created_at timestamptz NOT NULL DEFAULT now());

CREATE TABLE IF NOT EXISTS resources.submission_limits(owner_key text PRIMARY KEY, started_at timestamptz NOT NULL, used integer NOT NULL CHECK(used>0));
CREATE INDEX IF NOT EXISTS resources_submission_limits_started ON resources.submission_limits(started_at);


CREATE TABLE IF NOT EXISTS resources.audit_retention(singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton), revision bigint NOT NULL DEFAULT 0);
INSERT INTO resources.audit_retention(singleton) VALUES(true) ON CONFLICT DO NOTHING;
CREATE OR REPLACE FUNCTION resources.audit_project() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.audit_actor=COALESCE(NULLIF(NEW.data->>'Actor',''),'unknown'); NEW.audit_target=COALESCE(NULLIF(NEW.data->>'ResourceID',''),'unknown'); NEW.audit_action=NEW.data->>'Type'; NEW.audit_school=COALESCE(NULLIF(NEW.data->>'SchoolID',''),'*'); NEW.audit_time=(NEW.data->>'Time')::timestamptz; RETURN NEW; END $$;
CREATE OR REPLACE TRIGGER audit_project BEFORE INSERT OR UPDATE ON resources.audit FOR EACH ROW EXECUTE FUNCTION resources.audit_project();
CREATE OR REPLACE FUNCTION resources.audit_invalidate() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN UPDATE resources.audit_retention SET revision=revision+1 WHERE singleton; RETURN NULL; END $$;
REVOKE ALL ON FUNCTION resources.audit_invalidate() FROM PUBLIC;
CREATE OR REPLACE TRIGGER audit_invalidate AFTER UPDATE OR DELETE OR TRUNCATE ON resources.audit FOR EACH STATEMENT EXECUTE FUNCTION resources.audit_invalidate();
CREATE INDEX IF NOT EXISTS audit_query_order ON resources.audit(audit_time DESC,(id::text) COLLATE "C" DESC);

-- Immutable source versions outlive business deletion, so rebuilds include tombstones.
CREATE TABLE IF NOT EXISTS resources.discovery_history(id text NOT NULL, revision bigint NOT NULL CHECK(revision>0), data jsonb NOT NULL, recorded_xid xid8 NOT NULL DEFAULT pg_current_xact_id(), PRIMARY KEY(id,revision));
CREATE INDEX IF NOT EXISTS resources_discovery_page ON resources.discovery_history(id COLLATE "C",revision DESC);
CREATE TABLE IF NOT EXISTS resources.discovery_retention(singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton), revision bigint NOT NULL DEFAULT 0);
INSERT INTO resources.discovery_retention(singleton) VALUES(true) ON CONFLICT DO NOTHING;
CREATE OR REPLACE FUNCTION resources.invalidate_discovery() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN UPDATE resources.discovery_retention SET revision=revision+1 WHERE singleton; RETURN NULL; END $$;
REVOKE ALL ON FUNCTION resources.invalidate_discovery() FROM PUBLIC;
CREATE OR REPLACE TRIGGER invalidate_discovery AFTER UPDATE OR DELETE OR TRUNCATE ON resources.discovery_history FOR EACH STATEMENT EXECUTE FUNCTION resources.invalidate_discovery();
REVOKE ALL ON TABLE resources.discovery_history,resources.discovery_retention FROM PUBLIC;

CREATE TABLE IF NOT EXISTS resources.coverage(id text PRIMARY KEY,data jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS resources.purchases(id text PRIMARY KEY,owner_key text NOT NULL,resource_id text NOT NULL,data jsonb NOT NULL,query_changed_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE INDEX IF NOT EXISTS resources_purchase_owner ON resources.purchases(owner_key,resource_id);
CREATE TABLE IF NOT EXISTS resources.memberships(owner_key text NOT NULL,plan_id text NOT NULL,data jsonb NOT NULL,PRIMARY KEY(owner_key,plan_id));
CREATE TABLE IF NOT EXISTS resources.inbox(id text PRIMARY KEY,received_at timestamptz NOT NULL DEFAULT now());
REVOKE ALL ON TABLE resources.coverage,resources.purchases,resources.memberships,resources.inbox FROM PUBLIC;

CREATE TABLE IF NOT EXISTS resources.counter_shards(name text NOT NULL,shard smallint NOT NULL CHECK(shard>=0 AND shard<64),delta bigint NOT NULL DEFAULT 0,PRIMARY KEY(name,shard));
REVOKE ALL ON TABLE resources.counter_shards FROM PUBLIC;
INSERT INTO resources.counter_shards(name,shard,delta)
SELECT v.name,0,v.total FROM (VALUES
  ('records',(SELECT count(*) FROM resources.records)),
  ('replays',(SELECT count(*) FROM resources.replays)),
  ('active_jobs',(SELECT count(*) FROM resources.jobs WHERE state IN ('pending','working')))
) AS v(name,total)
WHERE NOT EXISTS (SELECT 1 FROM resources.counter_shards cs WHERE cs.name=v.name);

CREATE OR REPLACE FUNCTION resources.bump_counter(p_name text,p_key text,p_delta bigint) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN INSERT INTO resources.counter_shards(name,shard,delta) VALUES(p_name,pg_catalog.mod(pg_catalog.abs(pg_catalog.hashtext(p_key)::bigint),64)::smallint,p_delta) ON CONFLICT(name,shard) DO UPDATE SET delta=resources.counter_shards.delta+EXCLUDED.delta; END $$;
REVOKE ALL ON FUNCTION resources.bump_counter(text,text,bigint) FROM PUBLIC;

CREATE OR REPLACE FUNCTION resources.count_records_row() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN IF TG_OP='INSERT' THEN PERFORM resources.bump_counter('records',NEW.id,1); ELSIF TG_OP='DELETE' THEN PERFORM resources.bump_counter('records',OLD.id,-1); END IF; RETURN NULL; END $$;
REVOKE ALL ON FUNCTION resources.count_records_row() FROM PUBLIC;
DROP TRIGGER IF EXISTS resources_records_counter_row ON resources.records;
CREATE TRIGGER resources_records_counter_row AFTER INSERT OR DELETE ON resources.records FOR EACH ROW EXECUTE FUNCTION resources.count_records_row();

CREATE OR REPLACE FUNCTION resources.count_replays_row() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN IF TG_OP='INSERT' THEN PERFORM resources.bump_counter('replays',NEW.key,1); ELSIF TG_OP='DELETE' THEN PERFORM resources.bump_counter('replays',OLD.key,-1); END IF; RETURN NULL; END $$;
REVOKE ALL ON FUNCTION resources.count_replays_row() FROM PUBLIC;
DROP TRIGGER IF EXISTS resources_replays_counter_row ON resources.replays;
CREATE TRIGGER resources_replays_counter_row AFTER INSERT OR DELETE ON resources.replays FOR EACH ROW EXECUTE FUNCTION resources.count_replays_row();

CREATE OR REPLACE FUNCTION resources.count_jobs_row() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ DECLARE old_active boolean:=false; new_active boolean:=false; BEGIN IF TG_OP IN ('UPDATE','DELETE') THEN old_active:=OLD.state IN ('pending','working'); END IF; IF TG_OP IN ('INSERT','UPDATE') THEN new_active:=NEW.state IN ('pending','working'); END IF; IF new_active AND NOT old_active THEN PERFORM resources.bump_counter('active_jobs',NEW.id,1); ELSIF old_active AND NOT new_active THEN PERFORM resources.bump_counter('active_jobs',COALESCE(NEW.id,OLD.id),-1); END IF; RETURN NULL; END $$;
REVOKE ALL ON FUNCTION resources.count_jobs_row() FROM PUBLIC;
DROP TRIGGER IF EXISTS resources_jobs_counter_row ON resources.jobs;
CREATE TRIGGER resources_jobs_counter_row AFTER INSERT OR UPDATE OF state OR DELETE ON resources.jobs FOR EACH ROW EXECUTE FUNCTION resources.count_jobs_row();

CREATE OR REPLACE FUNCTION resources.reset_counter() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN DELETE FROM resources.counter_shards WHERE name=TG_ARGV[0]; INSERT INTO resources.counter_shards(name,shard,delta) VALUES(TG_ARGV[0],0,0); RETURN NULL; END $$;
REVOKE ALL ON FUNCTION resources.reset_counter() FROM PUBLIC;
DROP TRIGGER IF EXISTS resources_records_counter_truncate ON resources.records;
CREATE TRIGGER resources_records_counter_truncate AFTER TRUNCATE ON resources.records FOR EACH STATEMENT EXECUTE FUNCTION resources.reset_counter('records');
DROP TRIGGER IF EXISTS resources_replays_counter_truncate ON resources.replays;
CREATE TRIGGER resources_replays_counter_truncate AFTER TRUNCATE ON resources.replays FOR EACH STATEMENT EXECUTE FUNCTION resources.reset_counter('replays');
DROP TRIGGER IF EXISTS resources_jobs_counter_truncate ON resources.jobs;
CREATE TRIGGER resources_jobs_counter_truncate AFTER TRUNCATE ON resources.jobs FOR EACH STATEMENT EXECUTE FUNCTION resources.reset_counter('active_jobs');
