-- 0021_session_revocation: the console session ledger (KB-325). console_sessions
-- gains the server-side revocation stamp and the issue-time membership
-- snapshot: revoked_at stays NULL while the session is live and is stamped
-- once Logout (or token rotation via the user-level revoke) kills it — token
-- resolution must observe the stamp immediately. Membership is the JSON
-- array of the member's project ids at issue time, read through the same
-- shared project_members set the lease scan and run read scope consult;
-- it is ledger metadata only and never a source of authority. Pre-S2 rows
-- carry no snapshot (empty text) and no stamp (still live).

ALTER TABLE console_sessions ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
ALTER TABLE console_sessions ADD COLUMN IF NOT EXISTS membership TEXT NOT NULL DEFAULT '';