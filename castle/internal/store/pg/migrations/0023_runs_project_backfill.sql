-- 0023_runs_project_backfill: one-shot cutover-window backfill of run
-- dispatch provenance onto the project the board triggers dispatched into
-- (KB-332). 0019 shipped runs.project_id (KB-324 S1): runs minted after it
-- inherit the dispatching assignment's project_id, so every project_id-NULL
-- kanboard_* row is legacy history minted before the column existed.
--
-- Backfill policy (KB-332): runs whose workflow is kanboard_* (kanboard
-- trigger adapter dispatches, KB-320) join the kanboard-tickets project;
-- foreign harness/legacy workflows stay unscoped and remain
-- console-operator-visible only. The stamp is guarded on the kanboard-tickets
-- project row (created by the live 2026-10-25 seeding), so a deployment
-- without that project keeps every run unscoped. One-shot: the version
-- ledger never replays it; deploy in the cutover window only.

UPDATE runs SET project_id = 'kanboard-tickets'
WHERE project_id IS NULL
  AND workflow_name LIKE 'kanboard\_%' ESCAPE '\'
  AND EXISTS (SELECT 1 FROM projects p WHERE p.id = 'kanboard-tickets');