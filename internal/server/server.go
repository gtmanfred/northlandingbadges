// Package server exposes the app's HTTP surface: the scheduled poll trigger, an
// optional DiscGolfScene webhook, wallet pass downloads, and a health endpoint.
//
// There is no user-facing UI by design (spec §2) — every interaction happens over
// email and the wallet apps.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/dgs"
	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/poll"
	"github.com/northlanding/badges/internal/store"
)

// maxWebhookBytes caps webhook bodies; the payload is a handful of fields.
const maxWebhookBytes = 1 << 20

// Runner executes poll cycles and single registrations.
type Runner interface {
	RunCycle(ctx context.Context) (poll.Report, error)
	ProcessClassified(ctx context.Context, reg domain.Registration, passType domain.PassType) (poll.Outcome, error)
}

// Options configures the HTTP handler.
type Options struct {
	Config *config.Config
	Runner Runner
	Store  *store.Store
	Log    *slog.Logger
	// Version is reported by /healthz to identify the running build.
	Version string
}

// Server holds handler dependencies.
type Server struct {
	cfg     *config.Config
	runner  Runner
	store   *store.Store
	log     *slog.Logger
	version string
}

// New validates options and builds the server.
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, errors.New("server: config is required")
	}
	if opts.Runner == nil {
		return nil, errors.New("server: runner is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	return &Server{cfg: opts.Config, runner: opts.Runner, store: opts.Store, log: log, version: version}, nil
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /tasks/poll", s.handlePoll)
	mux.HandleFunc("POST /webhooks/discgolfscene", s.handleWebhook)
	mux.HandleFunc("GET /passes/{id}", s.handlePassDownload)
	return mux
}

// HealthResponse is the /healthz body. EMAIL_MODE is included so the post-deploy
// smoke test can catch a deploy accidentally left in dry_run or allowlist.
type HealthResponse struct {
	Status         string `json:"status"`
	EmailMode      string `json:"email_mode"`
	Version        string `json:"version"`
	ClubTimezone   string `json:"club_timezone"`
	AppleWallet    bool   `json:"apple_wallet_configured"`
	GoogleWallet   bool   `json:"google_wallet_configured"`
	IngestPolling  bool   `json:"ingest_polling_configured"`
	SchemaVersion  int    `json:"schema_version,omitempty"`
	ProcessedCount int    `json:"processed_count,omitempty"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{
		Status:        "ok",
		EmailMode:     string(s.cfg.EmailMode),
		Version:       s.version,
		ClubTimezone:  s.cfg.ClubTimezone.String(),
		AppleWallet:   s.cfg.Apple.Configured(),
		GoogleWallet:  s.cfg.Google.Configured(),
		IngestPolling: s.cfg.DGS.Configured(),
	}
	if resp.EmailMode == "" {
		resp.EmailMode = string(config.ModeLive)
	}

	if s.store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.store.Ping(ctx); err != nil {
			s.log.Error("healthz: database unreachable", "error", err)
			resp.Status = "degraded"
			writeJSON(w, http.StatusServiceUnavailable, resp)
			return
		}
		if v, err := s.store.SchemaVersion(ctx); err == nil {
			resp.SchemaVersion = v
		}
		if n, err := s.store.CountProcessed(ctx); err == nil {
			resp.ProcessedCount = n
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r, s.cfg.PollTriggerToken) {
		s.log.Warn("poll trigger rejected", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	report, err := s.runner.RunCycle(r.Context())
	if err != nil {
		s.log.Error("poll cycle failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  err.Error(),
			"report": report,
		})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// Fall back to the poll token when no dedicated webhook secret is set, so the
	// endpoint is never unauthenticated.
	secret := s.cfg.WebhookSecret
	if secret == "" {
		secret = s.cfg.PollTriggerToken
	}
	if !s.authorized(r, secret) {
		s.log.Warn("webhook rejected", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot read body"})
		return
	}

	reg, passType, err := dgs.ParseWebhook(body, s.cfg.ClubTimezone)
	if err != nil {
		s.log.Warn("webhook payload rejected", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	outcome, err := s.runner.ProcessClassified(r.Context(), reg, passType)
	if err != nil {
		s.log.Error("webhook processing failed", "registration_id", reg.ID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	status := http.StatusAccepted
	if outcome.Duplicate {
		// Already handled: report success so the sender does not retry forever.
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"registration_id": outcome.RegistrationID,
		"duplicate":       outcome.Duplicate,
		"action":          string(outcome.Action),
		"pass_type":       string(outcome.PassType),
		"expires_at":      formatTime(outcome.ExpiresAt),
	})
}

// handlePassDownload serves the .pkpass behind the "Add to Apple Wallet" button.
// The emailed URL's token is the capability that authorizes the download.
func (s *Server) handlePassDownload(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "pass storage unavailable", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSuffix(r.PathValue("id"), ".pkpass")
	token := r.URL.Query().Get("t")
	if id == "" || token == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	artifact, err := s.store.Artifact(r.Context(), id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("pass lookup failed", "registration_id", id, "error", err)
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if subtle.ConstantTimeCompare([]byte(artifact.AccessToken), []byte(token)) != 1 {
		s.log.Warn("pass download rejected: bad token", "registration_id", id)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if len(artifact.PKPass) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.pkpass")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "north-landing-"+id+".pkpass"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(artifact.PKPass); err != nil {
		s.log.Error("pass download write failed", "registration_id", id, "error", err)
	}
}

// authorized accepts the shared secret as a bearer token or an X-Poll-Token
// header, compared in constant time.
func (s *Server) authorized(r *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	presented := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if presented == "" {
		presented = strings.TrimSpace(r.Header.Get("X-Poll-Token"))
	}
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Default().Error("write json response", "error", err)
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
