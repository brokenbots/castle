# SQLite migrations

Use `NNNN_description.sql` for every migration file:

- `NNNN` is a 4-digit zero-padded integer, strictly monotonic with no gaps.
- Migrations are forward-only. To revert, ship a new migration that undoes the prior change.
- Every migration should be idempotent at the statement level where possible (`IF EXISTS` / `IF NOT EXISTS`), so retries remain safe when a run partially fails before transaction commit.
