-- CRI-142: persist the terminal failure reason on the run record so
-- operator-initiated terminal stamping (CancelRun, heartbeat-staleness
-- reaping) is visible through GetRun/ListRuns without inspecting events.
-- Nullable: agent-driven terminal events may carry no reason.
ALTER TABLE runs ADD COLUMN failure_reason TEXT;