package auth

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

func TestTokenFromHeaders(t *testing.T) {
	tests := []struct {
		name  string
		head  map[string]string
		want  string
		valid bool
	}{
		{name: "authorization bearer", head: map[string]string{"Authorization": "Bearer abc"}, want: "abc", valid: true},
		{name: "x overseer token", head: map[string]string{"X-Criteria-Token": "xyz"}, want: "xyz", valid: true},
		{name: "metadata token", head: map[string]string{"criteria-token": "meta"}, want: "meta", valid: true},
		{name: "invalid auth", head: map[string]string{"Authorization": "Basic abc"}, valid: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.head {
				h.Set(k, v)
			}
			got, ok := TokenFromHeaders(h)
			if ok != tc.valid {
				t.Fatalf("valid=%v want %v", ok, tc.valid)
			}
			if got != tc.want {
				t.Fatalf("token=%q want %q", got, tc.want)
			}
		})
	}
}

func TestConstantTimeEqual(t *testing.T) {
	token := "super-secret"
	hash := HashToken(token)
	if !ConstantTimeEqual(token, hash) {
		t.Fatal("expected token/hash to match")
	}
	if ConstantTimeEqual("wrong", hash) {
		t.Fatal("expected wrong token mismatch")
	}
	if ConstantTimeEqual(token, "abc") {
		t.Fatal("expected mismatch for invalid hash length")
	}
}

func TestValidateToken(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	if err := db.CreateOverseer(context.Background(), &store.Overseer{
		ID: "o1", Name: "name", TokenHash: HashToken("tok-1"), Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	ok, err := ValidateToken(context.Background(), db, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected valid token")
	}

	ok, err = ValidateToken(context.Background(), db, "tok-2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected invalid token")
	}
}
