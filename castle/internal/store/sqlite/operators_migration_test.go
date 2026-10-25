package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/migrate"
)

// TestMigrate_OperatorsUnifyLegacyCredentials drives the KB-330 unification
// migration over a realistic pre-0022 database: password console identities
// become password operators with the admin role, token-only class
// registries become alias operators with their class role, the alias
// columns are backfilled, the session ledger is re-keyed onto operator ids,
// and console_users is dropped.
func TestMigrate_OperatorsUnifyLegacyCredentials(t *testing.T) {
	db := newMigrationTestDB(t)
	ctx := context.Background()

	migs, err := migrate.Discover(migrationsFS)
	if err != nil {
		t.Fatalf("discover runtime migrations: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, m := range migs {
		if m.Version >= 22 {
			break
		}
		if _, err := db.ExecContext(ctx, string(m.SQL)); err != nil {
			t.Fatalf("apply %s: %v", m.Name, err)
		}
		// Record the version so the runner below treats the pre-0022
		// schema as already applied and runs only 0022.
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations(version) VALUES(?)`, m.Version); err != nil {
			t.Fatalf("record %s: %v", m.Name, err)
		}
	}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(store.TimestampLayout)
	seed := []string{
		// A console user (password identity) with one live login session.
		`INSERT INTO console_users(id,username,password_hash,created_at,updated_at)
		 VALUES ('cu-1','alice','$2a$10$consolehash','` + now + `','` + now + `')`,
		`INSERT INTO console_sessions(id,user_id,token_hash,created_at)
		 VALUES ('cs-1','cu-1','sess-hash-1','` + now + `')`,
		// Token-only class registries. The orchestrator deliberately shares
		// the console username ('alice'): the legacy names never shared a
		// namespace, and the alias seeding must tolerate that.
		`INSERT INTO overseers(id,name,hostname,version,token_hash,status,created_at,last_seen_at)
		 VALUES ('ovs-1','worker','h1','v1','agent-hash','online','` + now + `','` + now + `')`,
		`INSERT INTO orchestrators(id,name,token_hash,created_at)
		 VALUES ('orch-1','alice','orch-hash','` + now + `')`,
		`INSERT INTO triggers(id,token_hash,created_at)
		 VALUES ('trg-1','trg-hash','` + now + `')`,
	}
	for _, q := range seed {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v: %q", err, q)
		}
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Every legacy identity resolves to an operator row carrying its class
	// role, and the console user row is a password operator with the same id.
	type wantOp struct {
		id, name, attributes string
		password             bool
	}
	for _, w := range []wantOp{
		{id: "cu-1", name: "alice", attributes: `{"role":"admin"}`, password: true},
		{id: "ovs-1", name: "worker", attributes: `{"role":"agent"}`},
		{id: "orch-1", name: "alice", attributes: `{"role":"orchestrator"}`},
		{id: "trg-1", name: "trg-1", attributes: `{"role":"trigger"}`},
	} {
		var name, attrs string
		var password sql.NullString
		var createdAt string
		err := db.QueryRowContext(ctx,
			`SELECT name, attributes, password_hash, created_at FROM operators WHERE id = ?`, w.id).
			Scan(&name, &attrs, &password, &createdAt)
		if err != nil {
			t.Fatalf("operator %s: %v", w.id, err)
		}
		if name != w.name {
			t.Errorf("operator %s name = %q, want %q", w.id, name, w.name)
		}
		if attrs != w.attributes {
			t.Errorf("operator %s attributes = %q, want %q", w.id, attrs, w.attributes)
		}
		if w.password {
			if password.String != "$2a$10$consolehash" {
				t.Errorf("operator %s password_hash = %v, want the console hash", w.id, password)
			}
		} else {
			if password.Valid {
				t.Errorf("operator %s must be token-only, got password %q", w.id, password.String)
			}
		}
		if createdAt == "" {
			t.Errorf("operator %s must carry created_at", w.id)
		}
	}

	// Alias columns are backfilled onto the operator ids.
	for _, q := range []struct {
		table, id string
	}{
		{"overseers", "ovs-1"}, {"orchestrators", "orch-1"}, {"triggers", "trg-1"},
	} {
		var alias string
		if err := db.QueryRowContext(ctx, `SELECT operator_id FROM `+q.table+` WHERE id = ?`, q.id).Scan(&alias); err != nil {
			t.Fatalf("%s operator_id: %v", q.table, err)
		}
		if alias != q.id {
			t.Fatalf("%s.operator_id = %q, want %q", q.table, alias, q.id)
		}
	}

	// The session ledger is re-keyed: user_id became operator_id and the
	// live session survives pointing at its operator.
	var opID string
	if err := db.QueryRowContext(ctx, `SELECT operator_id FROM console_sessions WHERE id = 'cs-1'`).Scan(&opID); err != nil {
		t.Fatalf("re-keyed session: %v", err)
	}
	if opID != "cu-1" {
		t.Fatalf("session operator_id = %q, want cu-1", opID)
	}
	var userIDDropped int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('console_sessions') WHERE name = 'user_id'`).Scan(&userIDDropped); err != nil || userIDDropped != 0 {
		t.Fatalf("console_sessions must drop user_id: count=%d err=%v", userIDDropped, err)
	}

	// console_users is superseded and gone.
	var consoleUsersLeft int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'console_users'`).Scan(&consoleUsersLeft); err != nil || consoleUsersLeft != 0 {
		t.Fatalf("console_users must be dropped: count=%d err=%v", consoleUsersLeft, err)
	}

	// The login namespace stays unique among password rows only.
	_, err = db.ExecContext(ctx,
		`INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at)
		 VALUES ('cu-2','alice','second','$2a$10$other','{"role":"admin"}','`+now+`','`+now+`')`)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("second password operator named 'alice' must be rejected, got err=%v", err)
	}
	// A token-only alias with that name is still acceptable (NULL hash,
	// outside the login-namespace index).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at)
		 VALUES ('orch-2','alice','token alias',NULL,'{"role":"orchestrator"}','`+now+`','`+now+`')`); err != nil {
		t.Fatalf("token-only alias may share a name: %v", err)
	}
}
