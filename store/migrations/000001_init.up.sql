-- ModelDock schema. Status/state columns are text + CHECK rather than enums so
-- adding a state is a cheap, transactional migration.

CREATE TABLE environments (
    name              text PRIMARY KEY,
    namespace         text NOT NULL UNIQUE,
    requires_approval boolean NOT NULL
);

INSERT INTO environments (name, namespace, requires_approval) VALUES
    ('staging', 'modeldock-staging', false),
    ('prod',    'modeldock-prod',    true);

CREATE TABLE models (
    id          uuid PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE model_versions (
    id          uuid PRIMARY KEY,
    model_id    uuid NOT NULL REFERENCES models(id),
    version     text NOT NULL,
    image       text NOT NULL,
    port        integer NOT NULL,
    metadata    jsonb NOT NULL DEFAULT '{}',
    resources   jsonb NOT NULL,
    scaling     jsonb NOT NULL,
    state       text NOT NULL CHECK (state IN ('registered','staging','pending_approval','approved',
                                               'production','superseded','rolled_back','failed')),
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (model_id, version)
);

-- One row per revision of a model in an environment. The highest revision is
-- the desired state; older rows are the deployment history.
CREATE TABLE deployments (
    id          uuid PRIMARY KEY,
    model_id    uuid NOT NULL REFERENCES models(id),
    version_id  uuid NOT NULL REFERENCES model_versions(id),
    environment text NOT NULL REFERENCES environments(name),
    revision    integer NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('deploy','promote','rollback','auto_rollback')),
    status      text NOT NULL CHECK (status IN ('pending','progressing','ready','failed','superseded','rolled_back')),
    reason      text NOT NULL DEFAULT '',
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    ready_at    timestamptz,
    finished_at timestamptz,
    UNIQUE (model_id, environment, revision)
);
CREATE INDEX deployments_in_flight ON deployments (created_at) WHERE status IN ('pending','progressing');
CREATE INDEX deployments_by_version ON deployments (version_id);

CREATE TABLE approvals (
    id           uuid PRIMARY KEY,
    version_id   uuid NOT NULL REFERENCES model_versions(id),
    status       text NOT NULL CHECK (status IN ('pending','approved','rejected')),
    requested_by text NOT NULL,
    requested_at timestamptz NOT NULL DEFAULT now(),
    decided_by   text,
    decided_at   timestamptz,
    comment      text NOT NULL DEFAULT '',
    -- Defence in depth for the four-eyes rule enforced in auth.CanDecide.
    CONSTRAINT approvals_no_self_approval CHECK (decided_by IS NULL OR decided_by <> requested_by),
    CONSTRAINT approvals_decision_complete CHECK ((status = 'pending') = (decided_by IS NULL))
);
CREATE UNIQUE INDEX approvals_one_pending_per_version ON approvals (version_id) WHERE status = 'pending';

-- Immutable audit log. Rows are written in the same transaction as the state
-- change they describe and can never be updated or deleted.
CREATE TABLE audit_events (
    seq           bigserial PRIMARY KEY,
    event_id      uuid NOT NULL UNIQUE,
    type          text NOT NULL,
    actor         text NOT NULL,
    model         text NOT NULL DEFAULT '',
    version       text NOT NULL DEFAULT '',
    environment   text NOT NULL DEFAULT '',
    deployment_id uuid,
    payload       jsonb NOT NULL,
    occurred_at   timestamptz NOT NULL
);
CREATE INDEX audit_events_model ON audit_events (model, seq);

CREATE FUNCTION audit_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only (% rejected)', TG_OP;
END;
$$;
CREATE TRIGGER audit_events_no_update BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_immutable();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_immutable();

-- Transactional outbox: one row per event still to be published to Kafka.
CREATE TABLE outbox (
    id           bigserial PRIMARY KEY,
    event_id     uuid NOT NULL REFERENCES audit_events(event_id),
    topic        text NOT NULL,
    msg_key      text NOT NULL,
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    attempts     integer NOT NULL DEFAULT 0,
    last_error   text NOT NULL DEFAULT ''
);
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

-- Read model built by the dashboard consumer from Kafka.
CREATE TABLE dashboard_model_health (
    model                text PRIMARY KEY,
    last_deploy_version  text NOT NULL DEFAULT '',
    last_deploy_env      text NOT NULL DEFAULT '',
    last_deploy_status   text NOT NULL DEFAULT '',
    last_deploy_at       timestamptz,
    deploys_total        integer NOT NULL DEFAULT 0,
    deploys_failed       integer NOT NULL DEFAULT 0,
    rollbacks_total      integer NOT NULL DEFAULT 0,
    promotions_total     integer NOT NULL DEFAULT 0,
    last_health          text NOT NULL DEFAULT '',
    last_health_at       timestamptz,
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- Idempotency for the at-least-once consumer.
CREATE TABLE dashboard_processed_events (
    event_id     uuid PRIMARY KEY,
    processed_at timestamptz NOT NULL DEFAULT now()
);
