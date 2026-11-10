CREATE SCHEMA IF NOT EXISTS resources;
CREATE TABLE IF NOT EXISTS resources.categories (id text PRIMARY KEY, revision bigint NOT NULL, enabled boolean NOT NULL, data jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS resources.category_revisions (id text NOT NULL, revision bigint NOT NULL, data jsonb NOT NULL, PRIMARY KEY(id,revision));
CREATE TABLE IF NOT EXISTS resources.records (id text PRIMARY KEY, owner_key text NOT NULL, school_id text NOT NULL, state text NOT NULL, active_id text NOT NULL, proposal_id text NOT NULL, data jsonb NOT NULL, query_changed_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS resources_records_owner ON resources.records(owner_key,id);
CREATE INDEX IF NOT EXISTS resources_records_school ON resources.records(school_id,state,id);
CREATE TABLE IF NOT EXISTS resources.versions (id text PRIMARY KEY, resource_id text NOT NULL REFERENCES resources.records(id), number bigint NOT NULL, data jsonb NOT NULL, UNIQUE(resource_id,number));
CREATE TABLE IF NOT EXISTS resources.grants (id text PRIMARY KEY, resource_id text NOT NULL REFERENCES resources.records(id), subject_key text NOT NULL, data jsonb NOT NULL, query_changed_at timestamptz NOT NULL DEFAULT now(), UNIQUE(resource_id,subject_key));
CREATE TABLE IF NOT EXISTS resources.directories (id text PRIMARY KEY, school_id text NOT NULL, course_id text NOT NULL, parent_id text NOT NULL, data jsonb NOT NULL);
CREATE INDEX IF NOT EXISTS resources_directories_parent ON resources.directories(school_id,parent_id,id);
CREATE TABLE IF NOT EXISTS resources.packages (id text PRIMARY KEY, expires_at timestamptz NOT NULL, data jsonb NOT NULL);
CREATE INDEX IF NOT EXISTS resources_packages_expiry ON resources.packages(expires_at);
CREATE TABLE IF NOT EXISTS resources.requests (id text PRIMARY KEY, resource_id text NOT NULL REFERENCES resources.records(id), data jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS resources.jobs (id text PRIMARY KEY, state text NOT NULL, available_at timestamptz NOT NULL, lease_until timestamptz NOT NULL, data jsonb NOT NULL);
CREATE INDEX IF NOT EXISTS resources_jobs_claim ON resources.jobs(state,available_at,lease_until);
CREATE TABLE IF NOT EXISTS resources.replays (key text PRIMARY KEY, data jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS resources.audit (id text PRIMARY KEY, data jsonb NOT NULL, reason text NOT NULL DEFAULT '', result text NOT NULL DEFAULT 'unknown' CHECK(result IN ('success','denied','failed','unknown')), recorded_xid xid8 NOT NULL DEFAULT pg_current_xact_id(), audit_actor text NOT NULL DEFAULT 'unknown', audit_target text NOT NULL DEFAULT 'unknown', audit_action text NOT NULL DEFAULT 'unknown', audit_school text NOT NULL DEFAULT '*', audit_group text NOT NULL DEFAULT '*', audit_time timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS resources.outbox(id text PRIMARY KEY, data jsonb NOT NULL, seq bigint GENERATED ALWAYS AS IDENTITY UNIQUE, sent_at timestamptz);
CREATE INDEX IF NOT EXISTS resources_outbox_pending ON resources.outbox((data->>'source') COLLATE "C",(data->>'subject') COLLATE "C",seq) WHERE sent_at IS NULL;
REVOKE ALL ON SCHEMA resources FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA resources FROM PUBLIC;

CREATE INDEX IF NOT EXISTS resources_requests_reporter ON resources.requests((data->'Reporter'),id);

CREATE TABLE IF NOT EXISTS resources.closed_owners(owner_key text PRIMARY KEY,closure_id text NOT NULL,created_at timestamptz NOT NULL DEFAULT now());

CREATE TABLE IF NOT EXISTS resources.submission_limits(owner_key text PRIMARY KEY, started_at timestamptz NOT NULL, used integer NOT NULL CHECK(used>0));


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
