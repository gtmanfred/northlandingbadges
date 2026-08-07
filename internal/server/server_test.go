package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/poll"
	"github.com/northlanding/badges/internal/server"
	"github.com/northlanding/badges/internal/store"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeRunner struct {
	cycles    atomic.Int64
	processed atomic.Int64
	report    poll.Report
	outcome   poll.Outcome
	cycleErr  error
	procErr   error

	lastReg      atomic.Value // domain.Registration
	lastPassType atomic.Value // domain.PassType
}

func (f *fakeRunner) RunCycle(context.Context) (poll.Report, error) {
	f.cycles.Add(1)
	return f.report, f.cycleErr
}

func (f *fakeRunner) ProcessClassified(_ context.Context, reg domain.Registration, pt domain.PassType) (poll.Outcome, error) {
	f.processed.Add(1)
	f.lastReg.Store(reg)
	f.lastPassType.Store(pt)
	if f.procErr != nil {
		return poll.Outcome{}, f.procErr
	}
	out := f.outcome
	if out.RegistrationID == "" {
		out.RegistrationID = reg.ID
	}
	return out, nil
}

func testConfig(t *testing.T, mode config.EmailMode) *config.Config {
	t.Helper()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return &config.Config{
		PollTriggerToken: "poll-secret",
		WebhookSecret:    "hook-secret",
		EmailMode:        mode,
		ClubTimezone:     ny,
		BaseURL:          "https://badges.example.com",
	}
}

func newServer(t *testing.T, cfg *config.Config, runner server.Runner, st *store.Store) http.Handler {
	t.Helper()
	s, err := server.New(server.Options{Config: cfg, Runner: runner, Store: st, Log: quietLogger(), Version: "test-build"})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return s.Handler()
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "badges.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestHealthzReportsEmailMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.EmailMode{config.ModeLive, config.ModeAllowlist, config.ModeRedirect, config.ModeDryRun} {
		h := newServer(t, testConfig(t, mode), &fakeRunner{}, openStore(t))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("mode %q: status = %d", mode, rec.Code)
		}
		var body server.HealthResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.EmailMode != string(mode) {
			t.Errorf("email_mode = %q, want %q", body.EmailMode, mode)
		}
		if body.Status != "ok" {
			t.Errorf("status = %q", body.Status)
		}
		if body.Version != "test-build" {
			t.Errorf("version = %q", body.Version)
		}
		if body.ClubTimezone != "America/New_York" {
			t.Errorf("club_timezone = %q", body.ClubTimezone)
		}
		if body.SchemaVersion < 1 {
			t.Errorf("schema_version = %d, want the applied migration version", body.SchemaVersion)
		}
	}
}

func TestHealthzDefaultsUnsetModeToLive(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "")
	h := newServer(t, cfg, &fakeRunner{}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var body server.HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.EmailMode != string(config.ModeLive) {
		t.Errorf("email_mode = %q, want live", body.EmailMode)
	}
}

func TestHealthzReportsDegradedWhenDatabaseIsClosed(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, st)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestPollRequiresValidToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{name: "no token", headers: nil, want: http.StatusUnauthorized},
		{name: "wrong bearer", headers: map[string]string{"Authorization": "Bearer nope"}, want: http.StatusUnauthorized},
		{name: "empty bearer", headers: map[string]string{"Authorization": "Bearer "}, want: http.StatusUnauthorized},
		{name: "wrong header token", headers: map[string]string{"X-Poll-Token": "nope"}, want: http.StatusUnauthorized},
		{name: "valid bearer", headers: map[string]string{"Authorization": "Bearer poll-secret"}, want: http.StatusOK},
		{name: "valid header token", headers: map[string]string{"X-Poll-Token": "poll-secret"}, want: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{report: poll.Report{Fetched: 3, Processed: 1, Sent: 1}}
			h := newServer(t, testConfig(t, config.ModeLive), runner, nil)

			req := httptest.NewRequest(http.MethodPost, "/tasks/poll", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized {
				if runner.cycles.Load() != 0 {
					t.Error("unauthorized request still ran a poll cycle")
				}
				return
			}
			if runner.cycles.Load() != 1 {
				t.Fatalf("cycles = %d, want 1", runner.cycles.Load())
			}
			var report poll.Report
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatalf("decode report: %v", err)
			}
			if report.Fetched != 3 || report.Sent != 1 {
				t.Errorf("report = %+v", report)
			}
		})
	}
}

func TestPollRejectsEverythingWhenTokenUnset(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, config.ModeLive)
	cfg.PollTriggerToken = ""
	runner := &fakeRunner{}
	h := newServer(t, cfg, runner, nil)

	req := httptest.NewRequest(http.MethodPost, "/tasks/poll", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if runner.cycles.Load() != 0 {
		t.Error("ran a cycle with no configured token")
	}
}

func TestPollReportsCycleFailure(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{cycleErr: errors.New("roster returned 403")}
	h := newServer(t, testConfig(t, config.ModeLive), runner, nil)

	req := httptest.NewRequest(http.MethodPost, "/tasks/poll", nil)
	req.Header.Set("Authorization", "Bearer poll-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "403") {
		t.Errorf("body = %q, want the underlying error", rec.Body.String())
	}
}

func TestPollRejectsGET(t *testing.T) {
	t.Parallel()
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tasks/poll", nil)
	req.Header.Set("Authorization", "Bearer poll-secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestWebhookProcessesSimulatedPayload(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{outcome: poll.Outcome{Action: "sent", PassType: domain.PassTypeDay}}
	h := newServer(t, testConfig(t, config.ModeLive), runner, nil)

	body := strings.NewReader(`{
		"order_id": "DGS-99001",
		"name": "Casey Chains",
		"email": "casey@example.com",
		"item": "Day Pass",
		"purchased_at": "2026-07-04T10:15:00-04:00"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/discgolfscene", body)
	req.Header.Set("Authorization", "Bearer hook-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if runner.processed.Load() != 1 {
		t.Fatalf("processed = %d, want 1", runner.processed.Load())
	}
	reg := runner.lastReg.Load().(domain.Registration)
	if reg.ID != "DGS-99001" || reg.Email != "casey@example.com" {
		t.Errorf("registration = %+v", reg)
	}
	if pt := runner.lastPassType.Load().(domain.PassType); pt != domain.PassTypeDay {
		t.Errorf("pass type = %q", pt)
	}
}

func TestWebhookRequiresSecret(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	h := newServer(t, testConfig(t, config.ModeLive), runner, nil)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/discgolfscene", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if runner.processed.Load() != 0 {
		t.Error("unauthenticated webhook was processed")
	}
}

func TestWebhookFallsBackToPollTokenWhenNoSecretSet(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, config.ModeLive)
	cfg.WebhookSecret = ""
	runner := &fakeRunner{}
	h := newServer(t, cfg, runner, nil)

	body := strings.NewReader(`{"order_id":"DGS-1","name":"Casey","email":"c@x.com","item":"Day Pass","purchased_at":"2026-07-04T10:15:00-04:00"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/discgolfscene", body)
	req.Header.Set("Authorization", "Bearer poll-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
}

func TestWebhookRejectsUnparseablePayload(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	h := newServer(t, testConfig(t, config.ModeLive), runner, nil)

	for name, body := range map[string]string{
		"not json":     `{nope`,
		"unknown item": `{"order_id":"1","name":"C","email":"c@x.com","item":"Tournament Entry","purchased_at":"2026-07-04T10:15:00Z"}`,
		"no date":      `{"order_id":"1","name":"C","email":"c@x.com","item":"Day Pass"}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/webhooks/discgolfscene", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer hook-secret")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
	if runner.processed.Load() != 0 {
		t.Error("bad payloads reached the pipeline")
	}
}

func TestWebhookReportsDuplicateAsOK(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{outcome: poll.Outcome{Duplicate: true}}
	h := newServer(t, testConfig(t, config.ModeLive), runner, nil)

	body := strings.NewReader(`{"order_id":"DGS-1","name":"Casey","email":"c@x.com","item":"Day Pass","purchased_at":"2026-07-04T10:15:00-04:00"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/discgolfscene", body)
	req.Header.Set("Authorization", "Bearer hook-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a duplicate", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["duplicate"] != true {
		t.Errorf("body = %v, want duplicate=true", resp)
	}
}

func TestWebhookSurfacesPipelineFailure(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{procErr: errors.New("signing key missing")}
	h := newServer(t, testConfig(t, config.ModeLive), runner, nil)

	body := strings.NewReader(`{"order_id":"DGS-1","name":"Casey","email":"c@x.com","item":"Day Pass","purchased_at":"2026-07-04T10:15:00-04:00"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/discgolfscene", body)
	req.Header.Set("Authorization", "Bearer hook-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestPassDownloadRequiresMatchingToken(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.Claim(ctx, "DGS-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := st.SaveArtifact(ctx, store.Artifact{
		RegistrationID: "DGS-1", AccessToken: "good-token", PKPass: []byte("PK\x03\x04pass"),
	}); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, st)

	tests := []struct {
		name string
		url  string
		want int
	}{
		{name: "valid token", url: "/passes/DGS-1.pkpass?t=good-token", want: http.StatusOK},
		{name: "wrong token", url: "/passes/DGS-1.pkpass?t=bad-token", want: http.StatusNotFound},
		{name: "no token", url: "/passes/DGS-1.pkpass", want: http.StatusNotFound},
		{name: "unknown registration", url: "/passes/DGS-404.pkpass?t=good-token", want: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want != http.StatusOK {
				return
			}
			if got := rec.Header().Get("Content-Type"); got != "application/vnd.apple.pkpass" {
				t.Errorf("Content-Type = %q", got)
			}
			if !strings.Contains(rec.Header().Get("Content-Disposition"), "north-landing-DGS-1.pkpass") {
				t.Errorf("Content-Disposition = %q", rec.Header().Get("Content-Disposition"))
			}
			if rec.Body.String() != "PK\x03\x04pass" {
				t.Errorf("body = %q", rec.Body.String())
			}
		})
	}
}

func TestPassDownloadWithoutStore(t *testing.T) {
	t.Parallel()
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/passes/DGS-1.pkpass?t=x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()
	if _, err := server.New(server.Options{Runner: &fakeRunner{}}); err == nil {
		t.Error("expected error without config")
	}
	if _, err := server.New(server.Options{Config: testConfig(t, config.ModeLive)}); err == nil {
		t.Error("expected error without runner")
	}
}

// seedProcessed writes a finalized ledger row for the admin endpoint tests.
func seedProcessed(t *testing.T, st *store.Store, r store.Record) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Claim(ctx, r.RegistrationID); err != nil {
		t.Fatalf("Claim %s: %v", r.RegistrationID, err)
	}
	if err := st.MarkProcessed(ctx, r); err != nil {
		t.Fatalf("MarkProcessed %s: %v", r.RegistrationID, err)
	}
}

func adminRequest(method, target, token string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

type adminListResponse struct {
	Count         int `json:"count"`
	Registrations []struct {
		RegistrationID string `json:"registration_id"`
		Email          string `json:"email"`
		PassType       string `json:"pass_type"`
		ExpiresAt      string `json:"expires_at"`
		Action         string `json:"action"`
		ProcessedAt    string `json:"processed_at"`
	} `json:"registrations"`
}

func TestAdminListProcessedReturnsEmails(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	seedProcessed(t, st, store.Record{
		RegistrationID: "reg-1",
		Email:          "member@example.com",
		PassType:       "season_membership",
		ExpiresAt:      time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
		EmailMode:      "live",
		Action:         "sent",
		ProcessedAt:    time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	})
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, "/admin/processed", "poll-secret"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body %s", rec.Code, rec.Body.String())
	}
	var body adminListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 1 || len(body.Registrations) != 1 {
		t.Fatalf("count = %d, registrations = %d, want 1,1", body.Count, len(body.Registrations))
	}
	got := body.Registrations[0]
	if got.Email != "member@example.com" {
		t.Errorf("email = %q", got.Email)
	}
	if got.RegistrationID != "reg-1" || got.PassType != "season_membership" || got.Action != "sent" {
		t.Errorf("row = %+v", got)
	}
	if got.ExpiresAt != "2026-12-31T23:59:59Z" {
		t.Errorf("expires_at = %q", got.ExpiresAt)
	}
}

func TestAdminListProcessedFiltersByYear(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	seedProcessed(t, st, store.Record{
		RegistrationID: "reg-2025", Email: "a@example.com", Action: "sent",
		ExpiresAt:   time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
		ProcessedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	seedProcessed(t, st, store.Record{
		RegistrationID: "reg-2026", Email: "b@example.com", Action: "sent",
		ExpiresAt:   time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
		ProcessedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, "/admin/processed?year=2026", "poll-secret"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body adminListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 1 || body.Registrations[0].RegistrationID != "reg-2026" {
		t.Fatalf("body = %+v, want only reg-2026", body)
	}
}

func TestAdminListProcessedRejectsBadYear(t *testing.T) {
	t.Parallel()
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, openStore(t))
	for _, year := range []string{"nineteen", "0", "-5"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, "/admin/processed?year="+year, "poll-secret"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("year=%s: status = %d, want 400", year, rec.Code)
		}
	}
}

func TestAdminEndpointsRejectMissingAndWrongToken(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	seedProcessed(t, st, store.Record{RegistrationID: "reg-1", Email: "a@example.com", Action: "sent", ProcessedAt: time.Now()})
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, st)

	for _, tc := range []struct{ method, target, token string }{
		{http.MethodGet, "/admin/processed", ""},
		{http.MethodGet, "/admin/processed", "wrong"},
		{http.MethodDelete, "/admin/processed/reg-1", ""},
		{http.MethodDelete, "/admin/processed/reg-1", "wrong"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(tc.method, tc.target, tc.token))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s token=%q: status = %d, want 401", tc.method, tc.target, tc.token, rec.Code)
		}
	}

	// The row must survive every rejected delete.
	processed, err := st.Processed(context.Background(), "reg-1")
	if err != nil {
		t.Fatalf("Processed: %v", err)
	}
	if !processed {
		t.Error("unauthorized delete removed the row")
	}
}

func TestAdminTokenSupersedesPollToken(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, config.ModeLive)
	cfg.AdminToken = "admin-secret"
	h := newServer(t, cfg, &fakeRunner{}, openStore(t))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, "/admin/processed", "poll-secret"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("poll token status = %d, want 401 once ADMIN_TOKEN is set", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, "/admin/processed", "admin-secret"))
	if rec.Code != http.StatusOK {
		t.Errorf("admin token status = %d, want 200", rec.Code)
	}
}

func TestAdminDeleteProcessedRemovesRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t)
	seedProcessed(t, st, store.Record{RegistrationID: "reg-1", Email: "a@example.com", Action: "sent", ProcessedAt: time.Now()})
	if err := st.SaveArtifact(ctx, store.Artifact{RegistrationID: "reg-1", AccessToken: "tok", PKPass: []byte("pass")}); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodDelete, "/admin/processed/reg-1", "poll-secret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body %s", rec.Code, rec.Body.String())
	}

	processed, err := st.Processed(ctx, "reg-1")
	if err != nil {
		t.Fatalf("Processed: %v", err)
	}
	if processed {
		t.Error("registration still processed after delete")
	}

	// The pass link must stop working the moment the ledger row is gone.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/passes/reg-1?t=tok", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("pass download after delete = %d, want 404", rec.Code)
	}
}

func TestAdminDeleteUnknownRegistrationIs404(t *testing.T) {
	t.Parallel()
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, openStore(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodDelete, "/admin/processed/nope", "poll-secret"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAdminEndpointsWithoutStoreAreUnavailable(t *testing.T) {
	t.Parallel()
	h := newServer(t, testConfig(t, config.ModeLive), &fakeRunner{}, nil)
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/admin/processed"},
		{http.MethodDelete, "/admin/processed/reg-1"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(tc.method, tc.target, "poll-secret"))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", tc.method, tc.target, rec.Code)
		}
	}
}
