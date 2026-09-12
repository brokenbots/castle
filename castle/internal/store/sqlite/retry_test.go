package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"query error", errors.New("no such table: runs"), false},
		{"syntax error", errors.New(`syntax error near "FOO" (1)`), false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"interrupted", errors.New("interrupted (9)"), true},
		{"database is locked", errors.New("database is locked (5) (SQLITE_BUSY)"), true},
		{"database table is locked", errors.New("database table is locked (6)"), true},
		{"wrapped interrupted", fmt.Errorf("submit: %w", errors.New("interrupted (9)")), true},
	}
	for _, tc := range cases {
		if got := isTransient(tc.err); got != tc.want {
			t.Errorf("%s: isTransient = %v, want %v (err=%v)", tc.name, got, tc.want, tc.err)
		}
	}
}

// TestIsTransient_DriverBusyError checks code-based detection against a real
// modernc.org/sqlite error: a writer blocked past its busy_timeout surfaces
// SQLITE_BUSY as *sqlite.Error.
func TestIsTransient_DriverBusyError(t *testing.T) {
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "busy.db")
	holder, err := sql.Open("sqlite", fmt.Sprintf(openDSN, path))
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()
	if _, err := holder.Exec(`CREATE TABLE t(x INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	held, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer func() { _ = held.Rollback() }()
	if _, err := held.Exec(`INSERT INTO t VALUES (1)`); err != nil {
		t.Fatalf("hold write lock: %v", err)
	}

	blocked, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(1)", path))
	if err != nil {
		t.Fatalf("open blocked: %v", err)
	}
	defer blocked.Close()

	_, err = blocked.Exec(`INSERT INTO t VALUES (2)`)
	if err == nil {
		t.Fatal("expected SQLITE_BUSY from the blocked writer")
	}
	if !isTransient(err) {
		t.Fatalf("driver busy error not detected as transient: %v", err)
	}
	var serr *sqlite.Error
	if !errors.As(err, &serr) || serr.Code() != sqliteBusy {
		t.Fatalf("expected *sqlite.Error with SQLITE_BUSY code, got %T (%v)", err, err)
	}
}

func TestRetryOnTransient(t *testing.T) {
	t.Run("retries transient errors until success", func(t *testing.T) {
		var attempts int
		out, err := retryOnTransient(context.Background(), retryPolicy{attempts: 4, base: time.Millisecond, max: 2 * time.Millisecond},
			func() (int, error) {
				attempts++
				if attempts < 3 {
					return 0, errors.New("interrupted (9)")
				}
				return attempts, nil
			})
		if err != nil {
			t.Fatalf("retryOnTransient: %v", err)
		}
		if out != 3 || attempts != 3 {
			t.Fatalf("out = %d, attempts = %d, want out 3 attempts 3", out, attempts)
		}
	})

	t.Run("gives up after exhausting attempts", func(t *testing.T) {
		var attempts int
		_, err := retryOnTransient(context.Background(), retryPolicy{attempts: 3, base: time.Millisecond, max: time.Millisecond},
			func() (int, error) {
				attempts++
				return 0, errors.New("database is locked (5) (SQLITE_BUSY)")
			})
		if err == nil {
			t.Fatal("expected the transient error after exhausting attempts")
		}
		if attempts != 3 {
			t.Fatalf("attempts = %d, want 3", attempts)
		}
	})

	t.Run("does not retry non-transient errors", func(t *testing.T) {
		want := errors.New("no such table: runs")
		var attempts int
		_, err := retryOnTransient(context.Background(), retryPolicy{attempts: 5, base: time.Millisecond, max: time.Millisecond},
			func() (int, error) {
				attempts++
				return 0, want
			})
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (no retry for non-transient errors)", attempts)
		}
	})

	t.Run("stops retrying when context is cancelled during backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var attempts int
		_, err := retryOnTransient(ctx, retryPolicy{attempts: 10, base: 50 * time.Millisecond, max: 100 * time.Millisecond},
			func() (int, error) {
				attempts++
				if attempts == 1 {
					cancel()
				}
				return 0, errors.New("interrupted (9)")
			})
		if err == nil {
			t.Fatal("expected the transient error after cancellation")
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (no retry once context is done)", attempts)
		}
	})
}
