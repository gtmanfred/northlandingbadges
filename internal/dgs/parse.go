// Package dgs ingests registrations from DiscGolfScene.
//
// DiscGolfScene publishes no API contract, so two ingestion paths exist (spec §4):
// a webhook payload posted by the club manager backend (Option A), and the
// club-admin CSV export fetched via dgs.ExportClient and parsed by ParseExport
// (Option B). Both funnel into the same domain.Registration.
package dgs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/northlanding/badges/internal/domain"
)

// webhookPayload is the JSON shape accepted on the webhook endpoint. Field
// aliases are accepted because the exact key names DiscGolfScene emits are not
// documented; whichever arrives first wins.
type webhookPayload struct {
	OrderID        string `json:"order_id"`
	RegistrationID string `json:"registration_id"`
	ID             string `json:"id"`

	Name      string `json:"name"`
	FullName  string `json:"full_name"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`

	Email string `json:"email"`

	Item     string `json:"item"`
	PassType string `json:"pass_type"`
	Product  string `json:"product"`

	PurchasedAt string `json:"purchased_at"`
	CreatedAt   string `json:"created_at"`
	OrderDate   string `json:"order_date"`
}

// ParseWebhook converts a webhook body into a classified registration.
//
// The pass type is classified here so a payload the club sends with an unknown
// item is rejected loudly at the edge rather than mailing a wrong expiration.
func ParseWebhook(body []byte, loc *time.Location) (domain.Registration, domain.PassType, error) {
	if loc == nil {
		loc = time.UTC
	}
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return domain.Registration{}, "", fmt.Errorf("dgs: webhook payload is not JSON: %w", err)
	}

	reg := domain.Registration{
		ID:          firstNonEmpty(p.OrderID, p.RegistrationID, p.ID),
		Name:        firstNonEmpty(p.Name, p.FullName, strings.TrimSpace(p.FirstName+" "+p.LastName)),
		Email:       strings.TrimSpace(p.Email),
		RawPassType: firstNonEmpty(p.Item, p.PassType, p.Product),
	}

	rawDate := firstNonEmpty(p.PurchasedAt, p.CreatedAt, p.OrderDate)
	if rawDate == "" {
		return domain.Registration{}, "", errors.New("dgs: webhook payload has no purchase date")
	}
	purchasedAt, err := ParseDate(rawDate, loc)
	if err != nil {
		return domain.Registration{}, "", err
	}
	reg.PurchasedAt = purchasedAt
	// A season year in the item label is the only season signal a webhook carries;
	// without it a season membership is rejected rather than expiring by purchase
	// year.
	if year, ok := SeasonYearFromLabel(reg.RawPassType); ok {
		reg.SeasonYear = year
	}

	if err := reg.Validate(); err != nil {
		return domain.Registration{}, "", fmt.Errorf("dgs: %w", err)
	}
	passType, err := domain.ClassifyPassType(reg.RawPassType)
	if err != nil {
		return domain.Registration{}, "", fmt.Errorf("dgs: registration %s: %w", reg.ID, err)
	}
	return reg, passType, nil
}

// RowError records a single unusable row without aborting the whole page. One bad
// row must not block every other registrant's badge.
type RowError struct {
	Row int
	Err error
}

func (e RowError) Error() string { return fmt.Sprintf("row %d: %v", e.Row, e.Err) }
func (e RowError) Unwrap() error { return e.Err }

// dateLayouts are the formats seen on DiscGolfScene pages and webhook payloads,
// tried in order. RFC3339 first because webhooks are machine-generated.
var dateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"Jan 2, 2006 3:04 PM",
	"Jan 2, 2006 15:04",
	"Jan 2, 2006",
	"January 2, 2006 3:04 PM",
	"January 2, 2006",
	"01/02/2006 3:04 PM",
	"01/02/2006",
	"1/2/2006 3:04 PM",
	"1/2/2006",
}

// ParseDate interprets a DiscGolfScene date string in the club's timezone.
// Layouts without an offset are read as club-local, which is how the site
// presents them.
func ParseDate(raw string, loc *time.Location) (time.Time, error) {
	s := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if s == "" {
		return time.Time{}, errors.New("dgs: empty date")
	}
	if loc == nil {
		loc = time.UTC
	}
	// Normalize lowercase meridiems ("10:15 am") which time.Parse rejects.
	s = strings.Replace(s, " am", " AM", 1)
	s = strings.Replace(s, " pm", " PM", 1)

	for _, layout := range dateLayouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("dgs: unrecognized date %q", raw)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
