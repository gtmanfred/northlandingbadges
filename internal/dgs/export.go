package dgs

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/northlanding/badges/internal/domain"
)

// ErrMissingColumn means the export no longer carries a column the pipeline needs.
// The daily contract-check treats it as an upstream change.
var ErrMissingColumn = errors.New("dgs: export is missing a required column")

// exportColumns are the headers ParseExport requires, keyed by logical field.
var exportColumns = map[string]string{
	"division": "division",
	"name":     "name",
	"email":    "email",
	"date":     "registration date est",
}

// pdgaColumn is read when present but never required: an export without it must
// still produce badges, so it is deliberately kept out of exportColumns, which
// drives the ErrMissingColumn check.
const pdgaColumn = "pdga#"

// idLength is how much of the SHA-256 hex digest becomes the registration ID.
// Twelve hex characters is 48 bits — ample for a few hundred members a season,
// and short enough to print on a badge.
const idLength = 12

// RegistrationID derives a stable registration ID from the event and the
// registrant's email.
//
// The export carries no registration ID, and the pipeline needs one as its dedupe
// key, wallet serial and QR value. Email is already required to deliver a badge,
// so keying on it costs nothing. A blank email yields an empty ID, and the row is
// rejected upstream.
func RegistrationID(eventSlug, email string) string {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(eventSlug + "|" + normalized))
	return hex.EncodeToString(sum[:])[:idLength]
}

// ParseExport turns the club-admin CSV export into classified candidates.
//
// Returns the rows it could read plus a RowError per row it could not, so one
// unusable row never blocks the other registrants' badges. The trailing Totals
// row is dropped silently: it is a summary, not a registrant.
func ParseExport(r io.Reader, eventSlug string, seasonYear int, loc *time.Location) ([]domain.Candidate, []error) {
	if loc == nil {
		loc = time.UTC
	}
	if seasonYear == 0 {
		return nil, []error{errors.New("dgs: no season year configured")}
	}

	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1 // trailing summary rows are short

	header, err := reader.Read()
	if err != nil {
		return nil, []error{fmt.Errorf("dgs: read export header: %w", err)}
	}
	index := map[string]int{}
	for pos, cell := range header {
		norm := strings.ToLower(strings.Join(strings.Fields(cell), " "))
		for field, want := range exportColumns {
			if norm == want {
				index[field] = pos
			}
		}
		if norm == pdgaColumn {
			index[pdgaColumn] = pos
		}
	}
	for field := range exportColumns {
		if _, ok := index[field]; !ok {
			return nil, []error{fmt.Errorf("%w: %s", ErrMissingColumn, exportColumns[field])}
		}
	}

	var (
		out  []domain.Candidate
		errs []error
	)
	for row := 1; ; row++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			errs = append(errs, RowError{Row: row, Err: err})
			continue
		}
		at := func(field string) string {
			pos, ok := index[field]
			if !ok || pos >= len(record) {
				return ""
			}
			return strings.TrimSpace(record[pos])
		}

		division := at("division")
		if IsTotalsRow(division) {
			continue
		}
		passType, err := ClassifyDivision(division)
		if err != nil {
			errs = append(errs, RowError{Row: row, Err: err})
			continue
		}
		email := at("email")
		id := RegistrationID(eventSlug, email)
		if id == "" {
			errs = append(errs, RowError{Row: row, Err: fmt.Errorf("registration for %q: missing email", at("name"))})
			continue
		}
		purchasedAt, err := ParseDate(at("date"), loc)
		if err != nil {
			errs = append(errs, RowError{Row: row, Err: err})
			continue
		}

		// A PDGA number is optional and free-text upstream, so anything
		// non-numeric — or an all-digit value absurdly longer than any real
		// PDGA number — is dropped with a warning rather than failing the row.
		pdga := at(pdgaColumn)
		switch {
		case pdga == "":
			// nothing to validate
		case !isAllDigits(pdga):
			errs = append(errs, RowError{Row: row, Err: fmt.Errorf("ignoring non-numeric PDGA number %q", pdga)})
			pdga = ""
		case len(pdga) > maxPDGADigits:
			errs = append(errs, RowError{Row: row, Err: fmt.Errorf("ignoring implausibly long PDGA number %q", pdga)})
			pdga = ""
		}

		reg := domain.Registration{
			ID:          id,
			Name:        at("name"),
			Email:       email,
			RawPassType: division,
			PurchasedAt: purchasedAt,
			SeasonYear:  seasonYear,
			PDGANumber:  pdga,
		}
		if err := reg.Validate(); err != nil {
			errs = append(errs, RowError{Row: row, Err: err})
			continue
		}
		out = append(out, domain.Candidate{Registration: reg, PassType: passType})
	}
	return out, errs
}

// maxPDGADigits bounds how long an all-digit PDGA number is allowed to be.
// Real PDGA numbers are at most six digits today; ten leaves generous
// headroom without admitting absurd input. The cap exists because a
// thousands-of-digits paste-error would otherwise sail past isAllDigits and
// reach the QR encoder downstream (qrPanel in internal/badge/logo.go), whose
// "content too long to encode" error currently fails the whole registration —
// no email, no wallet passes — rather than just dropping the QR code.
const maxPDGADigits = 10

// isAllDigits reports whether s consists solely of ASCII digits.
func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
