package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/brokenbots/castle/castle/internal/store"
)

// BearerFromAuthorization parses an Authorization header and returns the bearer token.
func BearerFromAuthorization(v string) (string, bool) {
	if v == "" {
		return "", false
	}
	parts := strings.SplitN(v, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(parts[1])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// TokenFromHeaders extracts the auth token from supported headers.
func TokenFromHeaders(h http.Header) (string, bool) {
	if tok, ok := BearerFromAuthorization(h.Get("Authorization")); ok {
		return tok, true
	}
	if tok := strings.TrimSpace(h.Get("X-Criteria-Token")); tok != "" {
		return tok, true
	}
	if tok := strings.TrimSpace(h.Get("criteria-token")); tok != "" {
		return tok, true
	}
	return "", false
}

// HashToken computes the SHA-256 hex digest used for persisted token hashes.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// ConstantTimeEqual compares a token against a persisted hash using constant-time compare.
func ConstantTimeEqual(token, expectedHash string) bool {
	if expectedHash == "" {
		return false
	}
	got := HashToken(token)
	if len(got) != len(expectedHash) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expectedHash)) == 1
}

// ValidateToken checks whether token matches any overseer token hash in store.
func ValidateToken(ctx context.Context, st store.Store, token string) (bool, error) {
	o, err := ResolveToken(ctx, st, token)
	if err != nil {
		return false, err
	}
	return o != nil, nil
}

// ResolveToken returns the Overseer whose token matches, or nil if none matches.
func ResolveToken(ctx context.Context, st store.Store, token string) (*store.Overseer, error) {
	list, err := st.ListOverseers(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range list {
		if ConstantTimeEqual(token, o.TokenHash) {
			return o, nil
		}
	}
	return nil, nil
}
