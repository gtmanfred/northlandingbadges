package dgs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/domain"
)

// maxPageBytes caps how much of a fetched page is read, so a redirect to an
// unexpectedly huge page cannot exhaust a 256MB instance.
const maxPageBytes = 8 << 20

// Client fetches the club orders/roster page (Option B, the polling fallback).
type Client struct {
	cfg  config.DGSConfig
	http *http.Client
	loc  *time.Location
	log  *slog.Logger
}

// NewClient builds a poller. It keeps a cookie jar so a form login survives the
// subsequent roster request.
func NewClient(cfg config.DGSConfig, loc *time.Location, logger *slog.Logger) (*Client, error) {
	if !cfg.Configured() {
		return nil, fmt.Errorf("dgs: no roster URL configured")
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
	return &Client{
		cfg:  cfg,
		http: &http.Client{Jar: jar, Timeout: 30 * time.Second},
		loc:  loc,
		log:  logger,
	}, nil
}

// Fetch logs in if credentials are configured, then parses the roster page.
func (c *Client) Fetch(ctx context.Context) ([]domain.Registration, []error) {
	if c.cfg.LoginURL != "" && c.cfg.Username != "" {
		if err := c.login(ctx); err != nil {
			return nil, []error{err}
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.RosterURL, nil)
	if err != nil {
		return nil, []error{fmt.Errorf("dgs: build roster request: %w", err)}
	}
	req.Header.Set("User-Agent", "north-landing-badges/1.0 (+club automation)")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, []error{fmt.Errorf("dgs: fetch roster: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, []error{fmt.Errorf("dgs: roster returned %s", resp.Status)}
	}
	body := io.LimitReader(resp.Body, maxPageBytes)
	regs, errs := ParseOrders(body, c.loc)
	c.log.Info("fetched discgolfscene roster",
		"url", c.cfg.RosterURL, "registrations", len(regs), "row_errors", len(errs))
	return regs, errs
}

func (c *Client) login(ctx context.Context) error {
	form := url.Values{
		"username": {c.cfg.Username},
		"password": {c.cfg.Password},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.LoginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("dgs: build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "north-landing-badges/1.0 (+club automation)")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dgs: login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxPageBytes))

	if resp.StatusCode >= 400 {
		return fmt.Errorf("dgs: login returned %s", resp.Status)
	}
	return nil
}
