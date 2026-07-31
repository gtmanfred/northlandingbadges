package dgs_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/dgs"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const exportCSV = "Division,Name,Email,Registration date EST\n" +
	"MEM,Casey Chains,casey@example.com,2026-04-01 08:02:11\n" +
	"FNDR,Dana Discraft,dana@example.com,2025-11-13 01:07:26\n" +
	"Totals,,,\n"

// fakeDGS emulates sign-in plus the export, including session expiry.
type fakeDGS struct {
	logins     int
	exports    int
	expireOnce bool // first export after login fails as if the session lapsed
	badCreds   bool
	serveHTML  bool
}

func (f *fakeDGS) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/sign-in", func(w http.ResponseWriter, r *http.Request) {
		f.logins++
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if f.badCreds || r.PostFormValue("auth_email") != "admin@example.com" ||
			r.PostFormValue("auth_password") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("<html>bad credentials</html>"))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "dgs_session", Value: "ok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/tournaments/Slug/admin/registration-export", func(w http.ResponseWriter, r *http.Request) {
		f.exports++
		if _, err := r.Cookie("dgs_session"); err != nil || (f.expireOnce && f.exports == 1) {
			// DiscGolfScene answers an unauthenticated export with the sign-in page.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html><form class=\"form-signin\"></form></html>"))
			return
		}
		if f.serveHTML {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html>not csv</html>"))
			return
		}
		if err := r.ParseForm(); err != nil || r.PostFormValue("privacy_agree") != "1" {
			http.Error(w, "privacy_agree required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte(exportCSV))
	})
	return mux
}

func newFake(t *testing.T, f *fakeDGS) config.DGSConfig {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return config.DGSConfig{
		BaseURL: srv.URL, EventSlug: "Slug", SeasonYear: 2026,
		Email: "admin@example.com", Password: "secret",
	}
}

func TestExportClientFetch(t *testing.T) {
	t.Parallel()
	f := &fakeDGS{}
	client, err := dgs.NewExportClient(newFake(t, f), time.UTC, quietLogger())
	if err != nil {
		t.Fatalf("NewExportClient: %v", err)
	}
	candidates, errs := client.Fetch(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(candidates))
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1", f.logins)
	}

	// A second cycle reuses the session rather than logging in again.
	if _, errs := client.Fetch(context.Background()); len(errs) != 0 {
		t.Fatalf("second fetch errs = %v", errs)
	}
	if f.logins != 1 {
		t.Errorf("logins after two fetches = %d, want 1", f.logins)
	}
}

func TestExportClientRelogsInOnExpiredSession(t *testing.T) {
	t.Parallel()
	f := &fakeDGS{expireOnce: true}
	client, err := dgs.NewExportClient(newFake(t, f), time.UTC, quietLogger())
	if err != nil {
		t.Fatalf("NewExportClient: %v", err)
	}
	candidates, errs := client.Fetch(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none after re-login", errs)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(candidates))
	}
	if f.logins != 2 {
		t.Errorf("logins = %d, want 2 (initial plus retry)", f.logins)
	}
}

func TestExportClientBadCredentials(t *testing.T) {
	t.Parallel()
	f := &fakeDGS{badCreds: true}
	client, err := dgs.NewExportClient(newFake(t, f), time.UTC, quietLogger())
	if err != nil {
		t.Fatalf("NewExportClient: %v", err)
	}
	_, errs := client.Fetch(context.Background())
	if len(errs) == 0 {
		t.Fatal("Fetch with bad credentials returned no error")
	}
	if strings.Contains(errs[0].Error(), "secret") {
		t.Errorf("error leaks the password: %v", errs[0])
	}
	if strings.Contains(errs[0].Error(), "admin@example.com") {
		t.Errorf("error leaks the login email address: %v", errs[0])
	}
}

func TestExportClientHTMLInsteadOfCSVFailsAfterOneRetry(t *testing.T) {
	t.Parallel()
	f := &fakeDGS{serveHTML: true}
	client, err := dgs.NewExportClient(newFake(t, f), time.UTC, quietLogger())
	if err != nil {
		t.Fatalf("NewExportClient: %v", err)
	}
	if _, errs := client.Fetch(context.Background()); len(errs) == 0 {
		t.Fatal("Fetch returned no error when the export served HTML")
	}
	if f.exports > 2 {
		t.Errorf("exports = %d, want at most 2 (one attempt plus one retry)", f.exports)
	}
}

func TestNewExportClientRequiresConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := dgs.NewExportClient(config.DGSConfig{}, time.UTC, quietLogger()); err == nil {
		t.Fatal("NewExportClient with empty config = nil error, want an error")
	}
}
