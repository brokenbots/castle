-- CRI-187: persist the instant a run first entered the running state so the
-- run list can show endedAt - startedAt for finished runs and a live elapsed
-- time for running runs. Nullable: pending runs have not started yet.
ALTER TABLE runs ADD COLUMN started_at TEXT;

-- ListRuns keyset paging (CRI-187) orders by created_at DESC, id DESC; the
-- index lets each page read that ordering instead of re-sorting the table.
CREATE INDEX IF NOT EXISTS idx_runs_created_id ON runs(created_at DESC, id DESC);

-- Operational note (CRI-187): runs created before this migration, or that
-- never transitioned to running, keep started_at NULL; the run list's
-- duration column shows an em dash for them by design. No backfill intended.
