// Package store is the SQLite dedupe ledger. DiscGolfScene stays the source of
// truth for users; this database only remembers which registrations have already
// been handled, plus the wallet artifacts served over HTTP.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the image can be minimal
)

// ErrNotFound is returned when a registration or artifact row is absent.
var ErrNotFound = errors.New("store: not found")

// Store owns the SQLite connection.
type Store struct {
	db *sql.DB
}

// Record is the outcome of processing one registration.
type Record struct {
	RegistrationID string
	Email          string
	PassType       string
	ExpiresAt      time.Time
	EmailMode      string
	Action         string
	ProcessedAt    time.Time
}

// Artifact holds the generated wallet passes for a registration.
type Artifact struct {
	RegistrationID string
	AccessToken    string
	PKPass         []byte
	GoogleJWT      string
}

// Open opens (creating if needed) the database at path and applies migrations.
//
// Concurrency is deliberately serialized to a single connection: the workload is
// one hourly poll cycle, and a single writer removes any chance of SQLITE_BUSY on
// a Fly volume while keeping memory use flat.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for health checks.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// migration is one forward-only schema step. Steps are idempotent at the
// statement level and recorded in schema_migrations, so applying them to an
// empty volume and to a populated one both converge.
var migrations = []struct {
	version int
	stmts   []string
}{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS processed_registrations (
				registration_id TEXT PRIMARY KEY,
				email           TEXT NOT NULL DEFAULT '',
				pass_type       TEXT NOT NULL DEFAULT '',
				expires_at      TEXT NOT NULL DEFAULT '',
				email_mode      TEXT NOT NULL DEFAULT '',
				action          TEXT NOT NULL DEFAULT 'pending',
				processed_at    TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_processed_action ON processed_registrations(action)`,
		},
	},
	{
		version: 2,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS pass_artifacts (
				registration_id TEXT PRIMARY KEY,
				access_token    TEXT NOT NULL,
				pkpass          BLOB,
				google_jwt      TEXT NOT NULL DEFAULT '',
				created_at      TEXT NOT NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_token ON pass_artifacts(access_token)`,
		},
	},
	{
		version: 3,
		stmts: []string{
			// Founder badges are issued once per person, and the only stable
			// identifier across seasons is the email address.
			`CREATE INDEX IF NOT EXISTS idx_processed_email_pass_type
			   ON processed_registrations(email, pass_type)`,
		},
	},
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin migration %d: %w", m.version, err)
		}
		for _, stmt := range m.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("store: migration %d: %w", m.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, nowUTC(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

// SchemaVersion reports the highest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// Claim reserves a registration ID for processing. It returns true for the
// caller that won the race and false if the ID was already claimed, which makes
// it the dedupe gate: exactly one caller per registration proceeds to send mail.
func (s *Store) Claim(ctx context.Context, registrationID string) (bool, error) {
	if registrationID == "" {
		return false, errors.New("store: empty registration id")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO processed_registrations (registration_id, action, processed_at) VALUES (?, 'pending', ?)`,
		registrationID, nowUTC(),
	)
	if err != nil {
		return false, fmt.Errorf("store: claim %s: %w", registrationID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim %s: %w", registrationID, err)
	}
	return n == 1, nil
}

// Release removes a claim so a later cycle can retry. Used when the pipeline
// fails before a delivery decision was reached.
func (s *Store) Release(ctx context.Context, registrationID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pass_artifacts WHERE registration_id = ?`, registrationID); err != nil {
		return fmt.Errorf("store: release artifacts %s: %w", registrationID, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM processed_registrations WHERE registration_id = ?`, registrationID); err != nil {
		return fmt.Errorf("store: release %s: %w", registrationID, err)
	}
	return nil
}

// MarkProcessed finalizes a claimed registration with the guard's outcome.
//
// Suppressed deliveries (skipped, dry_run) are recorded exactly like sent ones:
// per spec §4 a guarded registration stays processed, so switching to live later
// cannot re-mail real users without deliberately clearing the row.
func (s *Store) MarkProcessed(ctx context.Context, r Record) error {
	if r.ProcessedAt.IsZero() {
		r.ProcessedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE processed_registrations
		    SET email = ?, pass_type = ?, expires_at = ?, email_mode = ?, action = ?, processed_at = ?
		  WHERE registration_id = ?`,
		r.Email, r.PassType, formatTime(r.ExpiresAt), r.EmailMode, r.Action,
		r.ProcessedAt.UTC().Format(time.RFC3339), r.RegistrationID,
	)
	if err != nil {
		return fmt.Errorf("store: mark processed %s: %w", r.RegistrationID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: mark processed %s: %w", r.RegistrationID, err)
	}
	if n == 0 {
		return fmt.Errorf("store: mark processed %s: %w", r.RegistrationID, ErrNotFound)
	}
	return nil
}

// Get returns the stored record for a registration.
func (s *Store) Get(ctx context.Context, registrationID string) (Record, error) {
	var (
		r         Record
		expiresAt string
		processed string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT registration_id, email, pass_type, expires_at, email_mode, action, processed_at
		   FROM processed_registrations WHERE registration_id = ?`, registrationID,
	).Scan(&r.RegistrationID, &r.Email, &r.PassType, &expiresAt, &r.EmailMode, &r.Action, &processed)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("store: %s: %w", registrationID, ErrNotFound)
	}
	if err != nil {
		return Record{}, fmt.Errorf("store: get %s: %w", registrationID, err)
	}
	r.ExpiresAt = parseTime(expiresAt)
	r.ProcessedAt = parseTime(processed)
	return r, nil
}

// Processed reports whether a registration has already been claimed.
func (s *Store) Processed(ctx context.Context, registrationID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM processed_registrations WHERE registration_id = ?`, registrationID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: processed %s: %w", registrationID, err)
	}
	return true, nil
}

// FounderIssued reports whether a founder badge has already been delivered to
// this email address. Founders re-register every season, but their badge never
// expires, so only the first registration produces one.
//
// Matching is case-insensitive because DiscGolfScene does not normalise the
// address. Rows still holding a pending claim do not count: nothing was issued
// for them yet. A blank email can never be matched and is reported as not
// issued, which pushes the row into the admin exception path instead of silently
// suppressing a badge.
func (s *Store) FounderIssued(ctx context.Context, email string) (bool, error) {
	trimmed := strings.TrimSpace(email)
	if trimmed == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM processed_registrations
		  WHERE lower(email) = lower(?) AND pass_type = 'founder' AND action <> 'pending'
		  LIMIT 1`, trimmed).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: founder issued %s: %w", trimmed, err)
	}
	return true, nil
}

// SaveArtifact stores the generated wallet passes, replacing any prior copy.
func (s *Store) SaveArtifact(ctx context.Context, a Artifact) error {
	if a.RegistrationID == "" || a.AccessToken == "" {
		return errors.New("store: artifact needs registration id and access token")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pass_artifacts (registration_id, access_token, pkpass, google_jwt, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(registration_id) DO UPDATE SET
			access_token = excluded.access_token,
			pkpass       = excluded.pkpass,
			google_jwt   = excluded.google_jwt,
			created_at   = excluded.created_at`,
		a.RegistrationID, a.AccessToken, a.PKPass, a.GoogleJWT, nowUTC(),
	)
	if err != nil {
		return fmt.Errorf("store: save artifact %s: %w", a.RegistrationID, err)
	}
	return nil
}

// Artifact looks up stored passes by registration ID.
func (s *Store) Artifact(ctx context.Context, registrationID string) (Artifact, error) {
	var a Artifact
	err := s.db.QueryRowContext(ctx,
		`SELECT registration_id, access_token, pkpass, google_jwt FROM pass_artifacts WHERE registration_id = ?`,
		registrationID,
	).Scan(&a.RegistrationID, &a.AccessToken, &a.PKPass, &a.GoogleJWT)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, fmt.Errorf("store: artifact %s: %w", registrationID, ErrNotFound)
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("store: artifact %s: %w", registrationID, err)
	}
	return a, nil
}

// CountProcessed returns how many registrations are on record; used by /healthz.
func (s *Store) CountProcessed(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM processed_registrations`).Scan(&n)
	return n, err
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
