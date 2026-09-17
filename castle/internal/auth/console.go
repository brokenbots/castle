package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"

	"golang.org/x/crypto/bcrypt"

	"github.com/brokenbots/castle/castle/internal/store"
)

// callerConsoleUserIDKey is the context key for the authenticated console
// user ID (CRI-195). Console identities are human operators logged in through
// the Login RPC with username + password. They are distinct from agent and
// orchestrator identities: CallerCriteriaID and CallerOrchestratorID stay
// empty for console callers, so handlers keyed on those never treat a console
// caller as an agent or orchestrator.
type callerConsoleUserIDKey struct{}

// CallerConsoleUserID returns the console user ID injected by
// AuthInterceptor, or "" when the caller is not a console user (agent token,
// orchestrator token, exempt procedure, or direct handler calls in tests
// without the interceptor wired).
func CallerConsoleUserID(ctx context.Context) string {
	v, _ := ctx.Value(callerConsoleUserIDKey{}).(string)
	return v
}

// WithCallerConsoleUserID returns a context with the given console user ID
// injected as the authenticated caller. Use in tests that call handlers
// directly (no HTTP stack) to simulate the identity the interceptor would
// inject.
func WithCallerConsoleUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callerConsoleUserIDKey{}, id)
}

// ResolveConsoleSession returns the console session whose persisted token
// hash matches, or nil when none matches (CRI-195). Compares against
// persisted SHA-256 hashes with constant-time equality like agent and
// orchestrator token resolution.
func ResolveConsoleSession(ctx context.Context, st store.Store, token string) (*store.ConsoleSession, error) {
	sessions, err := st.ListConsoleSessions(ctx)
	if err != nil {
		return nil, err
	}
	for _, sess := range sessions {
		if ConstantTimeEqual(token, sess.TokenHash) {
			return sess, nil
		}
	}
	return nil, nil
}

// NewSessionToken generates a fresh high-entropy session token for Login
// responses (CRI-195): 32 random bytes, URL-safe base64. Only its SHA-256
// hash is persisted.
func NewSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashPassword hashes a console-user password with bcrypt. Only the hash is
// persisted; the plaintext never touches the database or logs (CRI-195).
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword reports whether the plaintext password matches the stored
// bcrypt hash. Invalid hashes compare as mismatches.
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// DummyPasswordHash is a valid bcrypt hash used to equalize Login timing when
// the username is unknown: the same bcrypt work runs before the inevitable
// Unauthenticated response, so response latency does not reveal whether a
// username exists. It is not a credential — the comparison always fails.
const DummyPasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"