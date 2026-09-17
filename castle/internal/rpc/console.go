package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// ConsoleDefaultUserID is the fixed ID of the single seeded console user
// (CRI-195). Seeding is desired-state: re-seeding with a different
// CASTLE_CONSOLE_USER rotates the same row, so the previous username stops
// resolving and at most one console user ever exists.
const ConsoleDefaultUserID = "console-default"

// EnableConsoleLogin turns on the Login RPC (CRI-195). main.go calls this
// only when CASTLE_CONSOLE_USER and CASTLE_CONSOLE_PASSWORD are both set; it
// is never on by default.
func (s *ServerServer) EnableConsoleLogin() {
	s.consoleLogin = true
}

// ProvisionConsoleUser idempotently seeds the default console user from the
// operator's environment (CRI-195). If the user already exists with the same
// credentials it is a no-op; if the password changed, the hash is rotated and
// every existing console session is revoked so stale logins do not survive a
// credential rotation.
func ProvisionConsoleUser(ctx context.Context, st store.Store, username, password string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash console password: %w", err)
	}
	now := time.Now().UTC()
	existing, err := st.GetConsoleUser(ctx, username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if err := st.UpsertConsoleUser(ctx, &store.ConsoleUser{
			ID:           ConsoleDefaultUserID,
			Username:     username,
			PasswordHash: hash,
			CreatedAt:    now,
			UpdatedAt:    now,
		}); err != nil {
			return fmt.Errorf("seed console user: %w", err)
		}
		log.Info("console user seeded", "username", username)
	case err != nil:
		return fmt.Errorf("look up console user: %w", err)
	case !auth.VerifyPassword(existing.PasswordHash, password):
		if err := st.UpsertConsoleUser(ctx, &store.ConsoleUser{
			ID:           ConsoleDefaultUserID,
			Username:     username,
			PasswordHash: hash,
			CreatedAt:    existing.CreatedAt,
			UpdatedAt:    now,
		}); err != nil {
			return fmt.Errorf("rotate console user: %w", err)
		}
		// Rotation revokes sessions issued under the old password.
		if err := st.DeleteConsoleSessionsByUser(ctx, existing.ID); err != nil {
			return fmt.Errorf("revoke rotated console sessions: %w", err)
		}
		log.Info("console user password rotated; existing console sessions revoked", "username", username)
	default:
		// Already seeded with these credentials: idempotent no-op.
	}
	return nil
}

// RevokeConsoleAuth removes all console users and sessions (CRI-195). It is
// the fail-closed counterpart of seeding: when the console env vars are not
// configured, any stale console state is wiped so login is deterministically
// disabled.
func RevokeConsoleAuth(ctx context.Context, st store.Store) error {
	if err := st.DeleteConsoleUsers(ctx); err != nil {
		return fmt.Errorf("revoke console users: %w", err)
	}
	// The FK cascade should have removed the sessions; belt and braces for
	// deployments where the constraint is not enforced.
	return st.DeleteConsoleSessions(ctx)
}

// Login authenticates the default console user with username + password and
// issues a session token (CRI-195). It is exempt from the auth interceptor;
// this handler owns the feature gate: without CASTLE_CONSOLE_USER and
// CASTLE_CONSOLE_PASSWORD both set, login is disabled (Unimplemented) — never
// an open default.
func (s *ServerServer) Login(ctx context.Context, req *connect.Request[pb.LoginRequest]) (*connect.Response[pb.LoginResponse], error) {
	if !s.consoleLogin {
		return nil, connect.NewError(connect.CodeUnimplemented,
			errors.New("console login is disabled: set CASTLE_CONSOLE_USER and CASTLE_CONSOLE_PASSWORD to enable"))
	}
	if s.Log != nil {
		s.Log.Debug("console login attempt", "username", req.Msg.GetUsername())
	}
	if req.Msg.GetUsername() == "" || req.Msg.GetPassword() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("username and password are required"))
	}
	u, err := s.Store.GetConsoleUser(ctx, req.Msg.GetUsername())
	if errors.Is(err, store.ErrNotFound) {
		// Unknown username: burn the same bcrypt work a real verify would so
		// response timing does not reveal whether the username exists.
		auth.VerifyPassword(auth.DummyPasswordHash, req.Msg.GetPassword())
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid username or password"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !auth.VerifyPassword(u.PasswordHash, req.Msg.GetPassword()) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid username or password"))
	}
	token, err := auth.NewSessionToken()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	sess := &store.ConsoleSession{
		ID:        uuid.NewString(),
		UserID:    u.ID,
		TokenHash: auth.HashToken(token),
		CreatedAt: time.Now().UTC(),
	}
	if err := s.Store.CreateConsoleSession(ctx, sess); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if s.Log != nil {
		// Log the username only; never the password or the issued token.
		s.Log.Info("console login succeeded", "username", u.Username)
	}
	return connect.NewResponse(&pb.LoginResponse{
		SessionToken: token,
		Username:     u.Username,
	}), nil
}