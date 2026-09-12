-- CRI-131: k8s-native run metadata published by the criteria-k8s operator.
-- ticket is the external ticket identifier (e.g. "CRI-104"), repo_url the
-- repository the run operates on, and pr_url the pull request URL produced by
-- the run (published later via a run.metadata event). All nullable so
-- agent-initiated runs remain NULL until an orchestrator publishes metadata.
ALTER TABLE runs ADD COLUMN ticket TEXT;
ALTER TABLE runs ADD COLUMN repo_url TEXT;
ALTER TABLE runs ADD COLUMN pr_url TEXT;