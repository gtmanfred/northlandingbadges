package dgs_test

import (
	"testing"
	"time"

	"github.com/northlanding/badges/internal/dgs"
	"github.com/northlanding/badges/internal/domain"
)

func clubTZ(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

func TestParseWebhook(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	body := []byte(`{
		"order_id": "DGS-99001",
		"name": "Casey Chains",
		"email": "casey@example.com",
		"item": "Day Pass",
		"purchased_at": "2026-07-04T10:15:00-04:00"
	}`)
	reg, passType, err := dgs.ParseWebhook(body, ny)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if reg.ID != "DGS-99001" || reg.Name != "Casey Chains" || reg.Email != "casey@example.com" {
		t.Fatalf("registration = %+v", reg)
	}
	if passType != domain.PassTypeDay {
		t.Errorf("passType = %q, want day_pass", passType)
	}
	if want := time.Date(2026, 7, 4, 10, 15, 0, 0, ny); !reg.PurchasedAt.Equal(want) {
		t.Errorf("purchasedAt = %s, want %s", reg.PurchasedAt, want)
	}
}

func TestParseWebhookAcceptsFieldAliases(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"registration_id": "DGS-99002",
		"first_name": "Robin",
		"last_name": "Rollaway",
		"email": "robin@example.com",
		"pass_type": "2026 Season Membership",
		"created_at": "2026-04-01 08:02:00"
	}`)
	reg, passType, err := dgs.ParseWebhook(body, clubTZ(t))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if reg.ID != "DGS-99002" {
		t.Errorf("id = %q", reg.ID)
	}
	if reg.Name != "Robin Rollaway" {
		t.Errorf("name = %q, want first+last joined", reg.Name)
	}
	if passType != domain.PassTypeSeason {
		t.Errorf("passType = %q", passType)
	}
}

func TestParseWebhookRejectsBadPayloads(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"not json":       `{nope`,
		"missing id":     `{"name":"Casey","email":"c@x.com","item":"Day Pass","purchased_at":"2026-07-04T10:00:00Z"}`,
		"missing email":  `{"order_id":"1","name":"Casey","item":"Day Pass","purchased_at":"2026-07-04T10:00:00Z"}`,
		"missing name":   `{"order_id":"1","email":"c@x.com","item":"Day Pass","purchased_at":"2026-07-04T10:00:00Z"}`,
		"missing date":   `{"order_id":"1","name":"Casey","email":"c@x.com","item":"Day Pass"}`,
		"bad date":       `{"order_id":"1","name":"Casey","email":"c@x.com","item":"Day Pass","purchased_at":"soon"}`,
		"unknown item":   `{"order_id":"1","name":"Casey","email":"c@x.com","item":"Tournament Entry","purchased_at":"2026-07-04T10:00:00Z"}`,
		"no item at all": `{"order_id":"1","name":"Casey","email":"c@x.com","purchased_at":"2026-07-04T10:00:00Z"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := dgs.ParseWebhook([]byte(body), clubTZ(t)); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	tests := map[string]time.Time{
		"2026-07-04T10:15:00-04:00": time.Date(2026, 7, 4, 10, 15, 0, 0, ny),
		"2026-07-04 10:15:00":       time.Date(2026, 7, 4, 10, 15, 0, 0, ny),
		"2026-07-04":                time.Date(2026, 7, 4, 0, 0, 0, 0, ny),
		"Jul 4, 2026 10:15 AM":      time.Date(2026, 7, 4, 10, 15, 0, 0, ny),
		"Jul 4, 2026 10:15 am":      time.Date(2026, 7, 4, 10, 15, 0, 0, ny),
		"July 4, 2026":              time.Date(2026, 7, 4, 0, 0, 0, 0, ny),
		"07/04/2026 10:15 PM":       time.Date(2026, 7, 4, 22, 15, 0, 0, ny),
		"  Jul   4,  2026  ":        time.Date(2026, 7, 4, 0, 0, 0, 0, ny),
	}
	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			got, err := dgs.ParseDate(raw, ny)
			if err != nil {
				t.Fatalf("ParseDate(%q): %v", raw, err)
			}
			if !got.Equal(want) {
				t.Fatalf("ParseDate(%q) = %s, want %s", raw, got, want)
			}
		})
	}

	for _, bad := range []string{"", "   ", "tomorrow", "13/45/2026"} {
		if _, err := dgs.ParseDate(bad, ny); err == nil {
			t.Errorf("ParseDate(%q) should fail", bad)
		}
	}
}
