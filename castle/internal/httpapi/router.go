// Package httpapi exposes the v0 REST API.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/brokenbots/overlord/castle/internal/store"
)

type Server struct {
	Store   store.Store
	Log     *slog.Logger
	Control ControlService
}

type ControlService interface {
	StopRun(ctx context.Context, runID string) error
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Route("/api/v0", func(r chi.Router) {
		r.Post("/overseers/register", s.registerOverseer)
		r.Post("/overseers/{id}/heartbeat", s.heartbeat)
		r.Get("/overseers", s.listOverseers)
		r.Post("/overseers/{id}/runs", s.createRun)
		r.Get("/runs", s.listRuns)
		r.Get("/runs/{id}", s.getRun)
		r.Get("/runs/{id}/events", s.listEvents)
		r.Get("/runs/{id}/steps/{step}/logs", s.listStepLogs)
		r.Post("/runs/{id}/stop", s.stopRun)
	})
	return r
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type registerReq struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}
type registerResp struct {
	OverseerID string `json:"overseer_id"`
	Token      string `json:"token"`
}

func (s *Server) registerOverseer(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	id := uuid.NewString()
	tokenBytes := make([]byte, 24)
	_, _ = rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	o := &store.Overseer{
		ID: id, Name: req.Name, Hostname: req.Hostname, Version: req.Version,
		TokenHash: hex.EncodeToString(hash[:]), Status: "online",
		CreatedAt: now, LastSeenAt: now,
	}
	if err := s.Store.CreateOverseer(r.Context(), o); err != nil {
		s.Log.Error("create overseer", "err", err)
		writeErr(w, http.StatusInternalServerError, "store")
		return
	}
	writeJSON(w, http.StatusCreated, registerResp{OverseerID: id, Token: token})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.authorizeOverseer(w, r, id) {
		return
	}
	if err := s.Store.UpdateOverseerSeen(r.Context(), id, time.Now().UTC()); err != nil {
		writeErr(w, http.StatusInternalServerError, "store")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listOverseers(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListOverseers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, o := range list {
		out = append(out, map[string]any{
			"id": o.ID, "name": o.Name, "hostname": o.Hostname, "version": o.Version,
			"status": o.Status, "last_seen_at": o.LastSeenAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type createRunReq struct {
	WorkflowName string `json:"workflow_name"`
	WorkflowHCL  string `json:"workflow_hcl"`
}
type createRunResp struct {
	ID string `json:"id"`
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	overseerID := chi.URLParam(r, "id")
	if !s.authorizeOverseer(w, r, overseerID) {
		return
	}
	if _, err := s.Store.GetOverseer(r.Context(), overseerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "overseer not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "store")
		return
	}
	var req createRunReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.WorkflowName == "" {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	run := &store.Run{
		ID: id, OverseerID: overseerID, WorkflowName: req.WorkflowName,
		WorkflowHCL: req.WorkflowHCL, Status: "pending", CreatedAt: now,
	}
	if err := s.Store.CreateRun(r.Context(), run); err != nil {
		s.Log.Error("create run", "err", err)
		writeErr(w, http.StatusInternalServerError, "store")
		return
	}
	writeJSON(w, http.StatusCreated, createRunResp{ID: id})
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	overseerID := r.URL.Query().Get("overseer_id")
	status := r.URL.Query().Get("status")
	list, err := s.Store.ListRuns(r.Context(), overseerID, status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.Store.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "store")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.Store.ListEvents(r.Context(), id, since, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) listStepLogs(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	step := chi.URLParam(r, "step")
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.Store.ListStepLogs(r.Context(), runID, step, since, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	if s.Control == nil {
		writeErr(w, http.StatusNotImplemented, "stop control unavailable")
		return
	}
	runID := chi.URLParam(r, "id")
	if err := s.Control.StopRun(r.Context(), runID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancellation requested", "run_id": runID})
}

func (s *Server) authorizeOverseer(w http.ResponseWriter, r *http.Request, id string) bool {
	o, err := s.Store.GetOverseer(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "overseer not found")
			return false
		}
		writeErr(w, http.StatusInternalServerError, "store")
		return false
	}
	token := r.Header.Get("X-Overseer-Token")
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "missing token")
		return false
	}
	hash := sha256.Sum256([]byte(token))
	got := hex.EncodeToString(hash[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(o.TokenHash)) != 1 {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return false
	}
	return true
}
