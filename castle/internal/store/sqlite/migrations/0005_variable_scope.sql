-- 0005_variable_scope: add variable_scope column to runs (W04)
ALTER TABLE runs ADD COLUMN variable_scope TEXT NULL;
