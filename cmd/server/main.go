// Command server runs the North Landing badge distribution service.
//
// It is a single low-memory container that stays idle until something wakes it:
// the hourly GitHub Actions poll trigger, a DiscGolfScene webhook, or a wallet
// pass download. There is no in-container cron and no always-on worker, so the
// machine is free to scale to zero (spec §3).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "time/tzdata" // embed the tz database: the scratch image has no /usr/share/zoneinfo

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/dgs"
	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/mailer"
	"github.com/northlanding/badges/internal/poll"
	"github.com/northlanding/badges/internal/server"
	"github.com/northlanding/badges/internal/store"
	"github.com/northlanding/badges/internal/wallet/applepass"
	"github.com/northlanding/badges/internal/wallet/googlepass"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// Configuration is validated before anything else: an inconsistent delivery
	// guard must stop the deploy, not surprise a real registrant later (spec §4).
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Error("closing database", "error", err)
		}
	}()

	svc, err := buildService(cfg, db, log)
	if err != nil {
		return err
	}

	srv, err := server.New(server.Options{
		Config:  cfg,
		Runner:  svc,
		Store:   db,
		Log:     log,
		Version: version,
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// A poll cycle signs a pass and talks to SMTP per registration, so the
		// write timeout is generous relative to a normal web request.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	log.Info("starting server",
		"addr", cfg.Addr,
		"version", version,
		"email_mode", cfg.EmailMode,
		"club_timezone", cfg.ClubTimezone.String(),
		"apple_wallet", cfg.Apple.Configured(),
		"google_wallet", cfg.Google.Configured(),
		"ingest_polling", cfg.DGS.Configured(),
		"db_path", cfg.DBPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}

func buildService(cfg *config.Config, db *store.Store, log *slog.Logger) (*poll.Service, error) {
	guard, err := mailer.GuardFromConfig(cfg)
	if err != nil {
		return nil, err
	}

	var transport mailer.Transport
	if cfg.SendsMail() {
		transport = mailer.SMTPTransport{
			Addr:     cfg.SMTPAddr,
			Username: cfg.GmailUser,
			Password: cfg.GmailAppPassword,
		}
	}
	from := mail.Address{Name: cfg.FromName, Address: cfg.GmailUser}
	deliverer := mailer.New(guard, transport, from, log)

	svc := &poll.Service{
		Store:    db,
		Mailer:   deliverer,
		Location: cfg.ClubTimezone,
		BaseURL:  cfg.BaseURL,
		Log:      log,
	}

	if cfg.DGS.Configured() {
		client, err := dgs.NewClient(cfg.DGS, cfg.ClubTimezone, log)
		if err != nil {
			return nil, err
		}
		svc.Fetcher = client
	} else {
		// Webhook-only deployment (Option A). Poll cycles stay valid no-ops so the
		// hourly trigger and its smoke value are preserved.
		log.Warn("DGS_ROSTER_URL is unset: poll cycles will find no registrations; ingestion depends on the webhook")
		svc.Fetcher = emptyFetcher{}
	}

	if cfg.Apple.Configured() {
		signer, err := applepass.NewSigner(cfg.Apple)
		if err != nil {
			return nil, err
		}
		svc.Apple = signer
	} else {
		log.Warn("apple wallet is not configured: emails will ship without a .pkpass")
	}

	if cfg.Google.Configured() {
		issuer, err := googlepass.NewIssuer(cfg.Google)
		if err != nil {
			return nil, err
		}
		svc.Google = issuer
	} else {
		log.Warn("google wallet is not configured: emails will ship without a save link")
	}

	return svc, nil
}

// emptyFetcher stands in when roster polling is not configured.
type emptyFetcher struct{}

func (emptyFetcher) Fetch(context.Context) ([]domain.Registration, []error) { return nil, nil }
