-- 0001_init: base schema for the Postgres store profile (KB-317). Mirrors
-- the sqlite 0001 version's table set; types are adapted (TIMESTAMPTZ
-- instead of RFC3339Nano TEXT) and the events table is BORN time-partitioned:
-- one logical table, month-sharded storage, replacing the sqlite profile's
-- 46-shard per-run file growth.
CREATE TABLE IF NOT EXISTS overseers (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    hostname     TEXT,
    version      TEXT,
    token_hash   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'online',
    created_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id            TEXT PRIMARY KEY,
    overseer_id   TEXT REFERENCES overseers(id),
    workflow_name TEXT NOT NULL,
    workflow_hcl  TEXT NOT NULL,
    status        TEXT NOT NULL,
    current_step  TEXT NOT NULL,
    last_seq      BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL,
    ended_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_runs_overseer ON runs(overseer_id);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);

-- The events log is a single logical table partitioned by month. The
-- (run_id, seq, ts) primary key must carry the partition key; per-run seq
-- numbering is assigned inside the append transaction, and the correlation
-- dedup index (0002) carries at-most-once semantics.
CREATE TABLE IF NOT EXISTS events (
    run_id         TEXT NOT NULL,
    seq            BIGINT NOT NULL,
    type           TEXT NOT NULL,
    ts             TIMESTAMPTZ NOT NULL,
    correlation_id TEXT,
    payload        JSONB NOT NULL,
    PRIMARY KEY (run_id, seq, ts)
) PARTITION BY RANGE (ts);

-- Safety net so an append never fails when its month partition is missing;
-- castle_ensure_event_partition pre-creates explicit month partitions on
-- demand so this stays empty in steady state.
CREATE TABLE IF NOT EXISTS events_default PARTITION OF events DEFAULT;

-- Serialized (advisory-locked) on-demand month-partition creation.
CREATE OR REPLACE FUNCTION castle_ensure_event_partition(target TIMESTAMPTZ)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    part  TEXT     := 'events_m' || to_char(date_trunc('month', target), 'YYYYMM');
    lower TIMESTAMPTZ := date_trunc('month', target);
    upper TIMESTAMPTZ := lower + interval '1 month';
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('castle_event_partition_' || part));
    -- Resolve the name through the search path and require an actual table
    -- or partitioned-table relkind: a bare pg_class.relname probe would
    -- match a same-named index or a relation in another schema and silently
    -- suppress partition creation, pushing a whole month of rows into
    -- events_default permanently.
    IF to_regclass(part) IS NULL OR NOT EXISTS (
        SELECT 1 FROM pg_class
        WHERE oid = to_regclass(part)
          AND relkind IN ('r', 'p')
    ) THEN
        EXECUTE format('CREATE TABLE %I PARTITION OF events FOR VALUES FROM (%L) TO (%L)', part, lower, upper);
    END IF;
END $$;
