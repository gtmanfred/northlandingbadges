package dgs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/domain"
)

// maxExportBytes caps how much of a response is read, so an unexpected redirect
// to a huge page cannot exhaust a 256MB instance. The live 202-row export is a
// few tens of kilobytes.
const maxExportBytes = 8 << 20

// userAgent identifies this poller to DiscGolfScene.
const userAgent = "north-landing-badges/1.0 (+club automation)"

// ExportClient fetches the club-admin CSV registration export.
//
// DiscGolfScene publishes no API, so this drives the same form login and export
// POST a human would: the public roster page carries no email column, which makes
// the authenticated export the only source that can produce a badge.
type ExportClient struct {
	cfg  config.DGSConfig
	http *http.Client
	loc  *time.Location
	log  *slog.Logger

	// mu guards loggedIn: there is no session yet the first time Fetch runs, so
	// that first call must log in unconditionally rather than waiting to observe
	// a lapse that has not happened yet.
	mu       sync.Mutex
	loggedIn bool
}

// NewExportClient builds a poller with a cookie jar, so one login serves every
// cycle in the process's lifetime.
func NewExportClient(cfg config.DGSConfig, loc *time.Location, logger *slog.Logger) (*ExportClient, error) {
	if !cfg.Configured() {
		return nil, errors.New("dgs: export is not configured (need event slug, season year, email and password)")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("dgs: cookie jar: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	if loc == nil {
		loc = time.UTC
	}
	return &ExportClient{
		cfg:  cfg,
		http: &http.Client{Jar: jar, Timeout: 30 * time.Second},
		loc:  loc,
		log:  logger,
	}, nil
}

// Fetch logs in if needed, downloads the export and parses it.
//
// An export answered with HTML means the session lapsed (or, on the very first
// call, never existed), so the client logs in and retries exactly once. A second
// HTML response is an error: retrying further would just replay a broken
// assumption.
func (c *ExportClient) Fetch(ctx context.Context) ([]domain.Candidate, []error) {
	if !c.hasLoggedIn() {
		if err := c.login(ctx); err != nil {
			return nil, []error{err}
		}
		c.setLoggedIn()
	}

	body, err := c.export(ctx)
	if errors.Is(err, errNotCSV) {
		c.log.Info("discgolfscene session lapsed; logging in again")
		if loginErr := c.login(ctx); loginErr != nil {
			return nil, []error{loginErr}
		}
		body, err = c.export(ctx)
	}
	if err != nil {
		return nil, []error{err}
	}

	candidates, errs := ParseExport(strings.NewReader(body), c.cfg.EventSlug, c.cfg.SeasonYear, c.loc)
	c.log.Info("fetched discgolfscene export",
		"event", c.cfg.EventSlug, "season_year", c.cfg.SeasonYear,
		"candidates", len(candidates), "row_errors", len(errs))
	return candidates, errs
}

// errNotCSV signals that the export answered with something other than CSV, which
// in practice means the sign-in page.
var errNotCSV = errors.New("dgs: export did not return CSV")

func (c *ExportClient) hasLoggedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loggedIn
}

func (c *ExportClient) setLoggedIn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loggedIn = true
}

func (c *ExportClient) export(ctx context.Context) (string, error) {
	form := url.Values{"privacy_agree": {"1"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.ExportURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("dgs: build export request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("dgs: fetch export: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxExportBytes))
	if err != nil {
		return "", fmt.Errorf("dgs: read export: %w", err)
	}
	body := string(data)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("dgs: export returned %s", resp.Status)
	}
	if isHTML(resp.Header.Get("Content-Type"), body) {
		return "", errNotCSV
	}
	return body, nil
}

func (c *ExportClient) login(ctx context.Context) error {
	form := url.Values{
		"auth_email":    {c.cfg.Email},
		"auth_password": {c.cfg.Password},
		"redirect":      {""},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.LoginURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("dgs: build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dgs: login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxExportBytes))

	// The error deliberately names only the account, never the password.
	if resp.StatusCode >= 400 {
		return fmt.Errorf("dgs: DiscGolfScene sign-in returned %s", resp.Status)
	}
	return nil
}

// isHTML reports whether a response body is a web page rather than the CSV
// attachment — DiscGolfScene answers an unauthenticated export with the sign-in
// page and a 200.
func isHTML(contentType, body string) bool {
	if strings.Contains(strings.ToLower(contentType), "text/html") {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(body), "<")
}
