package dgs_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/dgs"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/orders.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

func TestClientFetchParsesRoster(t *testing.T) {
	t.Parallel()
	var rosterHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rosterHits, 1)
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "north-landing-badges") {
			t.Errorf("User-Agent = %q", got)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, fixture(t))
	}))
	defer srv.Close()

	client, err := dgs.NewClient(config.DGSConfig{RosterURL: srv.URL}, clubTZ(t), quietLogger())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	regs, errs := client.Fetch(context.Background())
	if len(regs) != 4 {
		t.Fatalf("parsed %d registrations, want 4 (errs: %v)", len(regs), errs)
	}
	if atomic.LoadInt32(&rosterHits) != 1 {
		t.Errorf("roster fetched %d times", rosterHits)
	}
}

func TestClientLogsInBeforeFetchingWhenCredentialsSet(t *testing.T) {
	t.Parallel()
	var loggedIn atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("login method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.Form.Get("username") != "club-admin" || r.Form.Get("password") != "hunter2" {
			t.Errorf("credentials = %q/%q", r.Form.Get("username"), r.Form.Get("password"))
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc", Path: "/"})
		loggedIn.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("session"); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, fixture(t))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, err := dgs.NewClient(config.DGSConfig{
		RosterURL: srv.URL + "/orders",
		LoginURL:  srv.URL + "/login",
		Username:  "club-admin",
		Password:  "hunter2",
	}, clubTZ(t), quietLogger())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	regs, errs := client.Fetch(context.Background())
	if !loggedIn.Load() {
		t.Fatal("client did not log in")
	}
	if len(regs) == 0 {
		t.Fatalf("no registrations parsed: %v", errs)
	}
}

func TestClientSurfacesHTTPErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	client, err := dgs.NewClient(config.DGSConfig{RosterURL: srv.URL}, clubTZ(t), quietLogger())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	regs, errs := client.Fetch(context.Background())
	if len(regs) != 0 {
		t.Errorf("got %d registrations from a 403", len(regs))
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "403") {
		t.Fatalf("errs = %v, want a 403 error", errs)
	}
}

func TestClientSurfacesLoginFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()

	client, err := dgs.NewClient(config.DGSConfig{
		RosterURL: srv.URL + "/orders",
		LoginURL:  srv.URL + "/login",
		Username:  "club-admin",
		Password:  "wrong",
	}, clubTZ(t), quietLogger())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, errs := client.Fetch(context.Background())
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "login") {
		t.Fatalf("errs = %v, want a login error", errs)
	}
}

func TestNewClientRequiresRosterURL(t *testing.T) {
	t.Parallel()
	if _, err := dgs.NewClient(config.DGSConfig{}, clubTZ(t), quietLogger()); err == nil {
		t.Fatal("expected error without a roster URL")
	}
}

func TestClientHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, fixture(t))
	}))
	defer srv.Close()

	client, err := dgs.NewClient(config.DGSConfig{RosterURL: srv.URL}, clubTZ(t), quietLogger())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, errs := client.Fetch(ctx); len(errs) == 0 {
		t.Fatal("expected an error from a cancelled context")
	}
}
