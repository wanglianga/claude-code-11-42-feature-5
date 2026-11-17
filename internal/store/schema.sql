-- Aviation cargo security event data-quality & status platform.
-- All source-of-truth tables are append-only; derived projections are rebuilt.

CREATE TABLE IF NOT EXISTS contracts (
    id              bigserial PRIMARY KEY,
    source          text        NOT NULL,
    message_type    text        NOT NULL,
    version         integer     NOT NULL,
    compat          text        NOT NULL CHECK (compat IN ('strict','backward','free')),
    definition      jsonb       NOT NULL,
    status          text        NOT NULL DEFAULT 'active',
    registered_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, message_type, version)
);

CREATE TABLE IF NOT EXISTS raw_events (
    seq                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id            uuid        NOT NULL UNIQUE,
    source              text        NOT NULL,
    message_type        text        NOT NULL,
    source_code         text,
    contract_version    integer     NOT NULL,
    idempotency_key     text        NOT NULL,
    dedupe_key          text        NOT NULL,
    business_keys       jsonb       NOT NULL,
    raw_business_keys   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    package_id          text,
    package_key         text,
    package_revision    text,
    shipment_key        text,
    device_id           text,
    device_event_no     text,
    evidence_indexes    jsonb       NOT NULL DEFAULT '[]'::jsonb,
    occurred_at         timestamptz NOT NULL,
    received_at         timestamptz NOT NULL,
    ingested_at         timestamptz NOT NULL DEFAULT now(),
    payload             jsonb       NOT NULL,
    content_hash        text        NOT NULL,
    supersedes_event_id uuid,
    missing_fields      jsonb       NOT NULL DEFAULT '[]'::jsonb,
    unmapped_fields     jsonb       NOT NULL DEFAULT '[]'::jsonb,
    UNIQUE (source, dedupe_key)
);

CREATE INDEX IF NOT EXISTS raw_events_received_idx ON raw_events (received_at, seq);
CREATE INDEX IF NOT EXISTS raw_events_package_idx  ON raw_events (package_key);
CREATE INDEX IF NOT EXISTS raw_events_shipment_idx ON raw_events (shipment_key);
CREATE INDEX IF NOT EXISTS raw_events_type_idx     ON raw_events (message_type);
CREATE INDEX IF NOT EXISTS raw_events_device_idx   ON raw_events (device_id);
CREATE INDEX IF NOT EXISTS raw_events_hashes_idx   ON raw_events (source, content_hash);

CREATE TABLE IF NOT EXISTS intake_attempts (
    id              bigserial PRIMARY KEY,
    source          text        NOT NULL,
    message_type    text        NOT NULL,
    source_code     text,
    idempotency_key text        NOT NULL,
    content_hash    text,
    occurred_at     timestamptz,
    received_at     timestamptz NOT NULL,
    outcome         text        NOT NULL CHECK (outcome IN
                        ('accepted','duplicate_identical','duplicate_conflicting','rejected')),
    detail          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    raw_event_id    uuid,
    attempted_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS intake_attempts_key_idx ON intake_attempts (source, idempotency_key);
CREATE INDEX IF NOT EXISTS intake_attempts_time_idx ON intake_attempts (received_at);

-- Identity graph. Links are never mutated; a link is closed by appending a
-- retraction, and splits/merges/container changes append new links. Every row
-- carries the knowledge time (received_at of the causing event) so snapshots
-- can be replayed against exactly the knowledge available at their watermark.
CREATE TABLE IF NOT EXISTS graph_links (
    id              bigserial PRIMARY KEY,
    link_key        text        NOT NULL,
    link_version    integer     NOT NULL,
    kind            text        NOT NULL,
    subject_type    text        NOT NULL,
    subject_key     text        NOT NULL,
    object_type     text        NOT NULL,
    object_key      text        NOT NULL,
    valid_from      timestamptz NOT NULL,
    knowledge_at    timestamptz NOT NULL,
    package_revision text,
    source_event_id uuid        NOT NULL,
    attributes      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (link_key, link_version)
);

CREATE INDEX IF NOT EXISTS graph_links_subject_idx ON graph_links (subject_type, subject_key);
CREATE INDEX IF NOT EXISTS graph_links_object_idx  ON graph_links (object_type, object_key);
CREATE INDEX IF NOT EXISTS graph_links_kind_idx    ON graph_links (kind);

CREATE TABLE IF NOT EXISTS graph_retractions (
    id              bigserial PRIMARY KEY,
    link_id         bigint      NOT NULL REFERENCES graph_links(id),
    retracted_at    timestamptz NOT NULL,
    knowledge_at    timestamptz NOT NULL,
    reason          text        NOT NULL,
    source_event_id uuid        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS graph_retractions_link_idx ON graph_retractions (link_id);

-- Disposition of each event against the identity graph: was the package still
-- active and at the referenced revision when the event arrived? Recomputed at
-- publish and appended idempotently.
CREATE TABLE IF NOT EXISTS evidence_dispositions (
    id              bigserial PRIMARY KEY,
    event_id        uuid        NOT NULL,
    package_key     text        NOT NULL DEFAULT '',
    shipment_key    text,
    disposition     text        NOT NULL,
    reason          text        NOT NULL,
    knowledge_at    timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, disposition, package_key)
);

CREATE INDEX IF NOT EXISTS evidence_dispositions_shipment_idx
    ON evidence_dispositions (shipment_key);

CREATE TABLE IF NOT EXISTS watermarks (
    id                      uuid        PRIMARY KEY,
    scope_type              text        NOT NULL,
    scope_key               text        NOT NULL,
    cutoff_received_at      timestamptz NOT NULL,
    required_sources        jsonb       NOT NULL,
    require_handover        boolean     NOT NULL DEFAULT true,
    mode                    text        NOT NULL CHECK (mode IN ('wait_complete','early_provisional')),
    label                   text,
    created_at              timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS watermark_closures (
    id              bigserial PRIMARY KEY,
    watermark_id    uuid        NOT NULL REFERENCES watermarks(id),
    source          text        NOT NULL,
    completed_at    timestamptz NOT NULL,
    note            text,
    recorded_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (watermark_id, source)
);

CREATE TABLE IF NOT EXISTS rule_violations (
    id                bigserial PRIMARY KEY,
    code              text        NOT NULL,
    severity          text        NOT NULL CHECK (severity IN ('info','warning','error','blocker')),
    subject_type      text        NOT NULL,
    subject_key       text        NOT NULL,
    dedupe_key        text        NOT NULL,
    knowledge_at      timestamptz NOT NULL,
    detail            jsonb       NOT NULL,
    related_event_ids jsonb       NOT NULL DEFAULT '[]'::jsonb,
    first_detected_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (code, subject_type, subject_key, dedupe_key)
);

CREATE INDEX IF NOT EXISTS rule_violations_subject_idx ON rule_violations (subject_type, subject_key);

CREATE TABLE IF NOT EXISTS status_assessments (
    id                  uuid        PRIMARY KEY,
    scope_type          text        NOT NULL,
    scope_key           text        NOT NULL,
    cutoff_received_at  timestamptz NOT NULL,
    max_seq             bigint      NOT NULL,
    status              text        NOT NULL,
    security_result     text,
    evidence_completeness double precision NOT NULL,
    result              jsonb       NOT NULL,
    inputs              jsonb       NOT NULL,
    result_hash         text        NOT NULL,
    computed_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS status_assessments_scope_idx ON status_assessments (scope_type, scope_key);

CREATE TABLE IF NOT EXISTS snapshots (
    id                    uuid        PRIMARY KEY,
    scope_type            text        NOT NULL,
    scope_key             text        NOT NULL,
    version               integer     NOT NULL,
    snapshot_kind         text        NOT NULL CHECK (snapshot_kind IN ('provisional','formal')),
    watermark_id          uuid        NOT NULL REFERENCES watermarks(id),
    assessment_id         uuid        NOT NULL REFERENCES status_assessments(id),
    cutoff_received_at    timestamptz NOT NULL,
    status                text        NOT NULL,
    security_result       text,
    evidence_completeness double precision NOT NULL,
    consumable_confirmed  boolean     NOT NULL,
    payload               jsonb       NOT NULL,
    quality_summary       jsonb       NOT NULL,
    diff                  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    supersedes_snapshot_id uuid,
    published_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope_type, scope_key, version)
);

CREATE INDEX IF NOT EXISTS snapshots_scope_idx ON snapshots (scope_type, scope_key, version);
CREATE INDEX IF NOT EXISTS snapshots_published_idx ON snapshots (published_at);

-- Append-only enforcement -----------------------------------------------------
CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'table %.% is append-only: % is not permitted', TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP;
END;
$$;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'contracts','raw_events','intake_attempts','graph_links','graph_retractions',
        'evidence_dispositions','watermarks','watermark_closures','rule_violations',
        'status_assessments','snapshots'
    ] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_trigger
            WHERE tgname = t || '_append_only' AND tgrelid = t::regclass
        ) THEN
            EXECUTE format(
                'CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION forbid_mutation()',
                t || '_append_only', t);
        END IF;
        IF NOT EXISTS (
            SELECT 1 FROM pg_trigger
            WHERE tgname = t || '_no_truncate' AND tgrelid = t::regclass
        ) THEN
            EXECUTE format(
                'CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation()',
                t || '_no_truncate', t);
        END IF;
    END LOOP;
END;
$$;

-- ---- Historical consistency review -----------------------------------------
-- Review material is append-only like the event ledger. Nothing here is derived
-- in place: an import appends records, a run appends results referencing the
-- exact material it read, and a re-analysis appends a new run. An old review is
-- therefore always reproducible from the records it pinned.

CREATE TABLE IF NOT EXISTS review_scopes (
    scope_id           text        PRIMARY KEY,
    shipment_key       text        NOT NULL,
    label              text,
    included_systems   jsonb       NOT NULL,
    included_streams   jsonb       NOT NULL DEFAULT '[]'::jsonb,
    authorized_subjects jsonb      NOT NULL DEFAULT '[]'::jsonb,
    excluded_systems   jsonb       NOT NULL DEFAULT '[]'::jsonb,
    cutoff_received_at timestamptz NOT NULL,
    note               text,
    registered_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS review_records (
    id              bigserial PRIMARY KEY,
    scope_id        text        NOT NULL REFERENCES review_scopes(scope_id),
    record_id       text        NOT NULL,
    system          text        NOT NULL,
    stream          text        NOT NULL,
    kind            text        NOT NULL,
    body            jsonb       NOT NULL,
    content_hash    text        NOT NULL,
    received_at     timestamptz NOT NULL,
    imported_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope_id, record_id)
);

CREATE INDEX IF NOT EXISTS review_records_scope_idx ON review_records (scope_id, record_id);

CREATE TABLE IF NOT EXISTS review_imports (
    id              bigserial PRIMARY KEY,
    scope_id        text        NOT NULL REFERENCES review_scopes(scope_id),
    batch_key       text        NOT NULL,
    imported_count  integer     NOT NULL,
    excluded        jsonb       NOT NULL DEFAULT '[]'::jsonb,
    late_arrivals   jsonb       NOT NULL DEFAULT '[]'::jsonb,
    imported_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope_id, batch_key)
);

CREATE TABLE IF NOT EXISTS review_cases (
    case_id          text        PRIMARY KEY,
    scope_id         text        NOT NULL REFERENCES review_scopes(scope_id),
    title            text,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS review_runs (
    run_id          uuid        PRIMARY KEY,
    case_id         text        NOT NULL REFERENCES review_cases(case_id),
    scope_id        text        NOT NULL,
    cutoff_received_at timestamptz NOT NULL,
    target          jsonb       NOT NULL,
    summary_verdict text        NOT NULL,
    budget_exhausted boolean    NOT NULL,
    material_ids    jsonb       NOT NULL,
    material_hash   text        NOT NULL,
    analysis        jsonb       NOT NULL,
    cross_check     jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS review_runs_case_idx ON review_runs (case_id, created_at);

CREATE TABLE IF NOT EXISTS review_examples (
    example_id      text        NOT NULL,
    run_id          uuid        NOT NULL REFERENCES review_runs(run_id) ON DELETE RESTRICT,
    case_id         text        NOT NULL,
    kind            text        NOT NULL,
    body            jsonb       NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, example_id)
);

-- Append-only enforcement for the review tables. Projections and derived
-- results are always new rows; the source-of-truth records may never be
-- rewritten, so an exported review and a later re-analysis cannot disagree due
-- to silent editing.
CREATE OR REPLACE FUNCTION forbid_review_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'table %.% is append-only: % is not permitted', TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP;
END;
$$;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'review_scopes','review_records','review_imports','review_cases',
        'review_runs','review_examples'
    ] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_trigger
                       WHERE tgname = t || '_append_only' AND tgrelid = t::regclass) THEN
            EXECUTE format(
                'CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION forbid_review_mutation()',
                t || '_append_only', t);
        END IF;
        IF NOT EXISTS (SELECT 1 FROM pg_trigger
                       WHERE tgname = t || '_no_truncate' AND tgrelid = t::regclass) THEN
            EXECUTE format(
                'CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION forbid_review_mutation()',
                t || '_no_truncate', t);
        END IF;
    END LOOP;
END;
$$;
