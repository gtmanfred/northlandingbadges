package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/store"
)

func openTemp(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "badges.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestMigrationsApplyToEmptyVolume(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	v, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v < 2 {
		t.Fatalf("schema version = %d, want >= 2", v)
	}
}

func TestMigrationsAreIdempotentOnPopulatedVolume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, path := openTemp(t)

	if _, err := s.Claim(ctx, "REG-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.MarkProcessed(ctx, store.Record{RegistrationID: "REG-1", Email: "a@b.com", Action: "sent"}); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen populated volume: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	ok, err := reopened.Processed(ctx, "REG-1")
	if err != nil {
		t.Fatalf("Processed: %v", err)
	}
	if !ok {
		t.Fatal("existing data lost across reopen")
	}
	n, err := reopened.CountProcessed(ctx)
	if err != nil {
		t.Fatalf("CountProcessed: %v", err)
	}
	if n != 1 {
		t.Fatalf("CountProcessed = %d, want 1", n)
	}
}

func TestClaimIsExactlyOncePerRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTemp(t)

	first, err := s.Claim(ctx, "REG-2")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !first {
		t.Fatal("first claim should win")
	}
	second, err := s.Claim(ctx, "REG-2")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if second {
		t.Fatal("replaying the same registration must not claim twice")
	}
}

func TestConcurrentClaimsYieldOneWinner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTemp(t)

	const workers = 16
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		errs  []error
		start = make(chan struct{})
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.Claim(ctx, "REG-RACE")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if ok {
				wins++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("claim errors: %v", errs)
	}
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
}

func TestClaimRejectsEmptyID(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	if _, err := s.Claim(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestMarkProcessedRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTemp(t)
	expires := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)

	if _, err := s.Claim(ctx, "REG-3"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	want := store.Record{
		RegistrationID: "REG-3",
		Email:          "casey@example.com",
		PassType:       "season_membership",
		ExpiresAt:      expires,
		EmailMode:      "allowlist",
		Action:         "skipped",
		ProcessedAt:    time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := s.MarkProcessed(ctx, want); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	got, err := s.Get(ctx, "REG-3")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Email != want.Email || got.PassType != want.PassType || got.Action != want.Action || got.EmailMode != want.EmailMode {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %s, want %s", got.ExpiresAt, expires)
	}
	if !got.ProcessedAt.Equal(want.ProcessedAt) {
		t.Errorf("ProcessedAt = %s, want %s", got.ProcessedAt, want.ProcessedAt)
	}
}

func TestMarkProcessedRequiresClaim(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	err := s.MarkProcessed(context.Background(), store.Record{RegistrationID: "ghost", Action: "sent"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetUnknownRegistration(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	if _, err := s.Get(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestReleaseAllowsRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTemp(t)

	if _, err := s.Claim(ctx, "REG-4"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.SaveArtifact(ctx, store.Artifact{RegistrationID: "REG-4", AccessToken: "tok", PKPass: []byte("zip")}); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}
	if err := s.Release(ctx, "REG-4"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	again, err := s.Claim(ctx, "REG-4")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !again {
		t.Fatal("released registration should be claimable again")
	}
	if _, err := s.Artifact(ctx, "REG-4"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("artifact should be gone, err = %v", err)
	}
}

func TestArtifactRoundTripAndReplace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTemp(t)

	if _, err := s.Claim(ctx, "REG-5"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.SaveArtifact(ctx, store.Artifact{
		RegistrationID: "REG-5", AccessToken: "tok-1", PKPass: []byte("pkpass-v1"), GoogleJWT: "jwt-1",
	}); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}
	if err := s.SaveArtifact(ctx, store.Artifact{
		RegistrationID: "REG-5", AccessToken: "tok-2", PKPass: []byte("pkpass-v2"), GoogleJWT: "jwt-2",
	}); err != nil {
		t.Fatalf("SaveArtifact replace: %v", err)
	}

	got, err := s.Artifact(ctx, "REG-5")
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	if got.AccessToken != "tok-2" || string(got.PKPass) != "pkpass-v2" || got.GoogleJWT != "jwt-2" {
		t.Fatalf("Artifact = %+v, want the replacement", got)
	}
}

func TestSaveArtifactValidates(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	if err := s.SaveArtifact(context.Background(), store.Artifact{AccessToken: "tok"}); err == nil {
		t.Error("expected error without registration id")
	}
	if err := s.SaveArtifact(context.Background(), store.Artifact{RegistrationID: "x"}); err == nil {
		t.Error("expected error without access token")
	}
}

func TestPing(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestFounderIssued(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTemp(t)

	if _, err := s.Claim(ctx, "reg-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.MarkProcessed(ctx, store.Record{
		RegistrationID: "reg-1",
		Email:          "Founder@Example.com",
		PassType:       "founder",
		Action:         "sent",
		ProcessedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	issued, err := s.FounderIssued(ctx, "founder@example.com")
	if err != nil {
		t.Fatalf("FounderIssued: %v", err)
	}
	if !issued {
		t.Error("FounderIssued for a processed founder = false, want true (match must be case-insensitive)")
	}

	issued, err = s.FounderIssued(ctx, "someone.else@example.com")
	if err != nil {
		t.Fatalf("FounderIssued: %v", err)
	}
	if issued {
		t.Error("FounderIssued for an unknown email = true, want false")
	}
}

func TestFounderIssuedIgnoresSeasonMembersAndPendingClaims(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTemp(t)

	// A season member with the same email is not a founder.
	if _, err := s.Claim(ctx, "reg-season"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.MarkProcessed(ctx, store.Record{
		RegistrationID: "reg-season",
		Email:          "dual@example.com",
		PassType:       "season_membership",
		Action:         "sent",
		ProcessedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	// A claimed-but-unprocessed founder row must not count as issued.
	if _, err := s.Claim(ctx, "reg-pending"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	issued, err := s.FounderIssued(ctx, "dual@example.com")
	if err != nil {
		t.Fatalf("FounderIssued: %v", err)
	}
	if issued {
		t.Error("FounderIssued = true for a season member and a pending claim, want false")
	}
}

func TestFounderIssuedBlankEmail(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	issued, err := s.FounderIssued(context.Background(), "   ")
	if err != nil {
		t.Fatalf("FounderIssued: %v", err)
	}
	if issued {
		t.Error("FounderIssued(blank) = true, want false: a blank email can never be deduped")
	}
}

func TestListProcessedReturnsNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTemp(t)

	seed(t, s, store.Record{
		RegistrationID: "reg-old",
		Email:          "old@example.com",
		PassType:       "season_membership",
		ExpiresAt:      time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
		Action:         "sent",
		ProcessedAt:    time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC),
	})
	seed(t, s, store.Record{
		RegistrationID: "reg-new",
		Email:          "new@example.com",
		PassType:       "season_membership",
		ExpiresAt:      time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
		Action:         "sent",
		ProcessedAt:    time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	})

	got, err := s.ListProcessed(ctx, store.ListFilter{})
	if err != nil {
		t.Fatalf("ListProcessed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].RegistrationID != "reg-new" || got[1].RegistrationID != "reg-old" {
		t.Errorf("order = %s,%s, want reg-new,reg-old", got[0].RegistrationID, got[1].RegistrationID)
	}
	if got[0].Email != "new@example.com" {
		t.Errorf("Email = %q, want new@example.com", got[0].Email)
	}
	if !got[0].ExpiresAt.Equal(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("ExpiresAt = %v, want 2026-12-31T23:59:59Z", got[0].ExpiresAt)
	}
}

func TestListProcessedFiltersByExpiryYear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTemp(t)

	seed(t, s, store.Record{
		RegistrationID: "reg-2025",
		Email:          "a@example.com",
		ExpiresAt:      time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
		Action:         "sent",
		ProcessedAt:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	seed(t, s, store.Record{
		RegistrationID: "reg-2026",
		Email:          "b@example.com",
		ExpiresAt:      time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
		Action:         "sent",
		ProcessedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	// Founder badges never expire, so they must not answer to any year filter.
	seed(t, s, store.Record{
		RegistrationID: "reg-founder",
		Email:          "c@example.com",
		PassType:       "founder",
		Action:         "sent",
		ProcessedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})

	got, err := s.ListProcessed(ctx, store.ListFilter{Year: 2026})
	if err != nil {
		t.Fatalf("ListProcessed: %v", err)
	}
	if len(got) != 1 || got[0].RegistrationID != "reg-2026" {
		t.Fatalf("got %v, want only reg-2026", ids(got))
	}
}

func TestDeleteProcessedRemovesRowAndArtifact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTemp(t)

	seed(t, s, store.Record{RegistrationID: "reg-1", Email: "a@example.com", Action: "sent", ProcessedAt: time.Now()})
	if err := s.SaveArtifact(ctx, store.Artifact{RegistrationID: "reg-1", AccessToken: "tok"}); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}

	if err := s.DeleteProcessed(ctx, "reg-1"); err != nil {
		t.Fatalf("DeleteProcessed: %v", err)
	}

	processed, err := s.Processed(ctx, "reg-1")
	if err != nil {
		t.Fatalf("Processed: %v", err)
	}
	if processed {
		t.Error("row still present after DeleteProcessed")
	}
	if _, err := s.Artifact(ctx, "reg-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Artifact err = %v, want ErrNotFound: the pass must not outlive the ledger row", err)
	}
}

func TestDeleteProcessedUnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	s, _ := openTemp(t)
	if err := s.DeleteProcessed(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteProcessed(unknown) = %v, want ErrNotFound", err)
	}
}

func seed(t *testing.T, s *store.Store, r store.Record) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Claim(ctx, r.RegistrationID); err != nil {
		t.Fatalf("Claim %s: %v", r.RegistrationID, err)
	}
	if err := s.MarkProcessed(ctx, r); err != nil {
		t.Fatalf("MarkProcessed %s: %v", r.RegistrationID, err)
	}
}

func ids(rs []store.Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.RegistrationID
	}
	return out
}
