package dgs_test

import (
	"errors"
	"os"
	"strings"
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

func TestParseOrdersFixture(t *testing.T) {
	t.Parallel()
	ny := clubTZ(t)
	f, err := os.Open("testdata/orders.html")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	regs, errs := dgs.ParseOrders(f, ny)

	// Four well-formed rows; the missing-email and bad-date rows are reported.
	wantIDs := []string{"DGS-88231", "DGS-88232", "DGS-88233", "DGS-88235"}
	if len(regs) != len(wantIDs) {
		t.Fatalf("parsed %d registrations (%v), want %d", len(regs), ids(regs), len(wantIDs))
	}
	for i, want := range wantIDs {
		if regs[i].ID != want {
			t.Errorf("registration[%d].ID = %q, want %q", i, regs[i].ID, want)
		}
	}
	if len(errs) != 2 {
		t.Errorf("row errors = %v, want 2 (missing email, bad date)", errs)
	}

	first := regs[0]
	if first.Name != "Casey Chains" {
		t.Errorf("name = %q", first.Name)
	}
	if first.Email != "casey@example.com" {
		t.Errorf("email = %q, want the mailto link text", first.Email)
	}
	if first.RawPassType != "North Landing DGC - Day Pass" {
		t.Errorf("raw pass type = %q", first.RawPassType)
	}
	want := time.Date(2026, 7, 4, 10, 15, 0, 0, ny)
	if !first.PurchasedAt.Equal(want) {
		t.Errorf("purchasedAt = %s, want %s", first.PurchasedAt, want)
	}

	// Classification must work on every parsed row, and reject the tournament entry.
	if pt, err := domain.ClassifyPassType(regs[0].RawPassType); err != nil || pt != domain.PassTypeDay {
		t.Errorf("row 0 classify = %q, %v", pt, err)
	}
	if pt, err := domain.ClassifyPassType(regs[1].RawPassType); err != nil || pt != domain.PassTypeSeason {
		t.Errorf("row 1 classify = %q, %v", pt, err)
	}
	if _, err := domain.ClassifyPassType(regs[3].RawPassType); !errors.Is(err, domain.ErrUnknownPassType) {
		t.Errorf("tournament entry should not classify: %v", err)
	}
}

func ids(regs []domain.Registration) []string {
	out := make([]string, len(regs))
	for i, r := range regs {
		out[i] = r.ID
	}
	return out
}

func TestParseOrdersIsHeaderDrivenNotPositional(t *testing.T) {
	t.Parallel()
	// Same data, columns reordered and renamed to a different valid alias set.
	page := `<table>
	<tr><th>Status</th><th>Purchase Date</th><th>E-Mail</th><th>Player Name</th><th>Product</th><th>Registration ID</th></tr>
	<tr><td>Paid</td><td>2026-07-04</td><td>casey@example.com</td><td>Casey Chains</td><td>Day Pass</td><td>DGS-1</td></tr>
	</table>`
	regs, errs := dgs.ParseOrders(strings.NewReader(page), clubTZ(t))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(regs) != 1 {
		t.Fatalf("parsed %d rows, want 1", len(regs))
	}
	got := regs[0]
	if got.ID != "DGS-1" || got.Name != "Casey Chains" || got.Email != "casey@example.com" || got.RawPassType != "Day Pass" {
		t.Fatalf("fields shifted: %+v", got)
	}
}

func TestParseOrdersSkipsUnrelatedTables(t *testing.T) {
	t.Parallel()
	page := `<table><tr><th>Metric</th><th>Value</th></tr><tr><td>Members</td><td>12</td></tr></table>`
	_, errs := dgs.ParseOrders(strings.NewReader(page), clubTZ(t))
	if len(errs) != 1 || !errors.Is(errs[0], dgs.ErrNoTable) {
		t.Fatalf("errs = %v, want ErrNoTable", errs)
	}
}

func TestParseOrdersReportsMarkupChange(t *testing.T) {
	t.Parallel()
	// This is the failure the daily contract-check workflow is meant to catch:
	// the orders table became a list of divs.
	page := `<div class="orders"><div class="order">DGS-1 Casey Chains casey@example.com Day Pass</div></div>`
	regs, errs := dgs.ParseOrders(strings.NewReader(page), clubTZ(t))
	if len(regs) != 0 {
		t.Errorf("parsed %d rows from unrecognised markup", len(regs))
	}
	if len(errs) != 1 || !errors.Is(errs[0], dgs.ErrNoTable) {
		t.Fatalf("errs = %v, want ErrNoTable", errs)
	}
}

func TestParseOrdersRowErrorCarriesRowNumber(t *testing.T) {
	t.Parallel()
	page := `<table>
	<tr><th>Order ID</th><th>Name</th><th>Email</th><th>Item</th><th>Date</th></tr>
	<tr><td>DGS-1</td><td>Casey</td><td>casey@example.com</td><td>Day Pass</td><td>2026-07-04</td></tr>
	<tr><td>DGS-2</td><td>Robin</td><td>robin@example.com</td><td>Day Pass</td><td>whenever</td></tr>
	</table>`
	regs, errs := dgs.ParseOrders(strings.NewReader(page), clubTZ(t))
	if len(regs) != 1 {
		t.Fatalf("parsed %d rows, want 1", len(regs))
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want 1", errs)
	}
	var rowErr dgs.RowError
	if !errors.As(errs[0], &rowErr) {
		t.Fatalf("err = %T, want RowError", errs[0])
	}
	if rowErr.Row != 2 {
		t.Errorf("RowError.Row = %d, want 2", rowErr.Row)
	}
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
