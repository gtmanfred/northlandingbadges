// Package dgs ingests registrations from DiscGolfScene.
//
// DiscGolfScene publishes no API contract, so two ingestion paths exist (spec §4):
// a webhook payload posted by the club manager backend (Option A), and a scrape
// of the club orders/roster page (Option B). Both funnel into the same
// domain.Registration, and the scraper is header-driven rather than
// position-driven so a reordered column does not silently shift fields.
package dgs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/northlanding/badges/internal/domain"
)

// ErrNoTable is returned when the fetched page has no recognisable orders table.
// The daily contract-check workflow treats it as an upstream markup change.
var ErrNoTable = errors.New("dgs: no orders table found")

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

// header aliases: the left side is what we need, the right side is what the page
// might call it.
var headerAliases = map[string][]string{
	"id":    {"order id", "order #", "order", "registration id", "registration", "id", "confirmation"},
	"name":  {"name", "player", "player name", "member", "guest", "purchaser"},
	"email": {"email", "e-mail", "email address"},
	"item":  {"item", "pass", "pass type", "product", "membership", "type", "description"},
	"date":  {"date", "purchased", "purchase date", "order date", "created", "registered"},
}

// ParseOrders extracts registrations from the club orders/roster page.
//
// Returns the rows it could read plus a RowError per row it could not, so a
// partially broken page still delivers the badges it can.
func ParseOrders(r io.Reader, loc *time.Location) ([]domain.Registration, []error) {
	if loc == nil {
		loc = time.UTC
	}
	doc, err := html.Parse(r)
	if err != nil {
		return nil, []error{fmt.Errorf("dgs: parse html: %w", err)}
	}

	for _, table := range findAll(doc, "table") {
		rows := findAll(table, "tr")
		if len(rows) < 2 {
			continue
		}
		index := headerIndex(rows[0])
		if index == nil {
			continue
		}

		var (
			out  []domain.Registration
			errs []error
		)
		for i, row := range rows[1:] {
			cells := rowCells(row)
			if len(cells) == 0 {
				continue
			}
			reg, err := registrationFromRow(cells, index, loc)
			if err != nil {
				errs = append(errs, RowError{Row: i + 1, Err: err})
				continue
			}
			out = append(out, reg)
		}
		if len(out) == 0 && len(errs) == 0 {
			continue
		}
		return out, errs
	}
	return nil, []error{ErrNoTable}
}

// headerIndex maps our logical field names onto column positions, or nil if the
// row does not look like an orders header.
func headerIndex(headerRow *html.Node) map[string]int {
	cells := rowCells(headerRow)
	if len(cells) == 0 {
		return nil
	}
	index := map[string]int{}
	for pos, cell := range cells {
		norm := normalizeHeader(cell)
		for field, aliases := range headerAliases {
			if _, taken := index[field]; taken {
				continue
			}
			for _, alias := range aliases {
				if norm == alias {
					index[field] = pos
					break
				}
			}
		}
	}
	// Without an ID, a name/email and an item we cannot build a badge, so this is
	// not the table we want.
	for _, required := range []string{"id", "name", "email", "item", "date"} {
		if _, ok := index[required]; !ok {
			return nil
		}
	}
	return index
}

func registrationFromRow(cells []string, index map[string]int, loc *time.Location) (domain.Registration, error) {
	at := func(field string) string {
		pos, ok := index[field]
		if !ok || pos >= len(cells) {
			return ""
		}
		return strings.TrimSpace(cells[pos])
	}

	reg := domain.Registration{
		ID:          at("id"),
		Name:        at("name"),
		Email:       at("email"),
		RawPassType: at("item"),
	}
	rawDate := at("date")
	if rawDate == "" {
		return domain.Registration{}, errors.New("missing purchase date")
	}
	purchasedAt, err := ParseDate(rawDate, loc)
	if err != nil {
		return domain.Registration{}, err
	}
	reg.PurchasedAt = purchasedAt

	if err := reg.Validate(); err != nil {
		return domain.Registration{}, err
	}
	return reg, nil
}

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

func findAll(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == tag {
			out = append(out, node)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// rowCells returns the trimmed text of each th/td directly inside a row.
func rowCells(row *html.Node) []string {
	var out []string
	for c := row.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || (c.Data != "td" && c.Data != "th") {
			continue
		}
		out = append(out, textOf(c))
	}
	return out
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func normalizeHeader(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ":")
	return strings.Join(strings.Fields(s), " ")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
