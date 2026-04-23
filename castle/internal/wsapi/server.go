// Package wsapi handles the bidirectional WebSocket. Overseers connect to
// /api/v0/ws?overseer_id=&token=, send envelopes, and the Castle assigns seq,
// persists, and fans out via the hub. Web clients connect to
// /api/v0/runs/{id}/stream and receive a live tail of events for that run.
package wsapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"

	"github.com/brokenbots/overlord/castle/internal/hub"
	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/events"
)

type Server struct {
	Store store.Store
	Hub   *hub.Hub
	Log   *slog.Logger

	mu        sync.RWMutex
	overseers map[string]*overseerConn
}

type overseerConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

type controlMessage struct {
	Type   string `json:"type"`
	RunID  string `json:"run_id"`
	Reason string `json:"reason,omitempty"`
}

func (s *Server) ensureState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overseers == nil {
		s.overseers = make(map[string]*overseerConn)
	}
}

func (s *Server) Mount(r chi.Router) {
	r.Get("/api/v0/ws", s.overseerWS)
	r.Get("/api/v0/runs/{id}/stream", s.clientStream)
}

// overseerWS ingests events from a connected Overseer.
func (s *Server) overseerWS(w http.ResponseWriter, r *http.Request) {
	overseerID := r.URL.Query().Get("overseer_id")
	if overseerID == "" {
		http.Error(w, "overseer_id required", http.StatusBadRequest)
		return
	}
	o, err := s.Store.GetOverseer(r.Context(), overseerID)
	if err != nil {
		http.Error(w, "unknown overseer", http.StatusUnauthorized)
		return
	}
	token := r.Header.Get("X-Overseer-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	hash := sha256.Sum256([]byte(token))
	got := hex.EncodeToString(hash[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(o.TokenHash)) != 1 {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Phase 0: dev-friendly. Lock down later.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.Log.Error("ws accept", "err", err)
		return
	}
	defer c.Close(websocket.StatusInternalError, "shutdown")

	s.ensureState()
	s.mu.Lock()
	s.overseers[overseerID] = &overseerConn{conn: c}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.overseers, overseerID)
		s.mu.Unlock()
	}()

	ctx := r.Context()
	for {
		var env events.Envelope
		if err := wsjson.Read(ctx, c, &env); err != nil {
			s.Log.Info("ws closed", "overseer_id", overseerID, "err", err)
			return
		}
		if env.SchemaVersion != events.SchemaVersion {
			s.Log.Warn("schema version mismatch", "got", env.SchemaVersion, "want", events.SchemaVersion)
			continue
		}
		if env.RunID == "" || env.Type == "" {
			s.Log.Warn("invalid envelope: missing run_id or type")
			continue
		}
		if env.Timestamp.IsZero() {
			env.Timestamp = time.Now().UTC()
		}
		seq, err := s.Store.AppendEvent(ctx, env)
		if err != nil {
			s.Log.Error("append event", "err", err, "run_id", env.RunID)
			continue
		}
		env.Seq = seq

		// Apply run-status side effects from terminal events.
		s.applyRunStatus(ctx, env)

		s.Hub.Publish(env)
	}
}

func (s *Server) StopRun(ctx context.Context, runID string) error {
	run, err := s.Store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	s.mu.RLock()
	oc := s.overseers[run.OverseerID]
	s.mu.RUnlock()
	if oc == nil {
		return fmt.Errorf("overseer %s not connected", run.OverseerID)
	}
	msg := controlMessage{Type: "run.cancel", RunID: runID, Reason: "requested by operator"}
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if err := wsjson.Write(ctx, oc.conn, msg); err != nil {
		return fmt.Errorf("send run.cancel: %w", err)
	}
	return nil
}

func (s *Server) applyRunStatus(ctx context.Context, env events.Envelope) {
	switch env.Type {
	case events.TypeRunStarted:
		run, err := s.Store.GetRun(ctx, env.RunID)
		if err != nil {
			return
		}
		run.Status = "running"
		var p events.RunStarted
		if json.Unmarshal(env.Payload, &p) == nil {
			run.CurrentStep = p.InitialStep
		}
		_ = s.Store.UpdateRun(ctx, run)
	case events.TypeStepEntered:
		run, err := s.Store.GetRun(ctx, env.RunID)
		if err != nil {
			return
		}
		var p events.StepEntered
		if json.Unmarshal(env.Payload, &p) == nil {
			run.CurrentStep = p.Step
			_ = s.Store.UpdateRun(ctx, run)
		}
	case events.TypeRunCompleted, events.TypeRunFailed:
		run, err := s.Store.GetRun(ctx, env.RunID)
		if err != nil {
			return
		}
		now := time.Now().UTC()
		run.EndedAt = &now
		if env.Type == events.TypeRunCompleted {
			var p events.RunCompleted
			if json.Unmarshal(env.Payload, &p) == nil && p.Success {
				run.Status = "succeeded"
			} else {
				run.Status = "failed"
			}
		} else {
			run.Status = "failed"
		}
		_ = s.Store.UpdateRun(ctx, run)
	}
}

// clientStream streams events for a single run to a web client.
func (s *Server) clientStream(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.Log.Error("ws accept", "err", err)
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "bye")

	sub := s.Hub.Subscribe(runID)
	defer s.Hub.Unsubscribe(sub)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C:
			if !ok {
				return
			}
			if err := wsjson.Write(ctx, c, env); err != nil {
				if !errors.Is(err, context.Canceled) {
					s.Log.Info("client stream closed", "err", err)
				}
				return
			}
		}
	}
}
