package email_test

import (
	"bytes"
	"flag"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/email"
)

var update = flag.Bool("update", false, "rewrite golden files")

func testData() email.Data {
	ny, _ := time.LoadLocation("America/New_York")
	return email.Data{
		Badge: domain.Badge{
			Registration: domain.Registration{
				ID:          "DGS-88231",
				Name:        "Casey Chains",
				Email:       "casey@example.com",
				RawPassType: "Day Pass",
				PurchasedAt: time.Date(2026, 7, 4, 10, 0, 0, 0, ny),
			},
			PassType:  domain.PassTypeDay,
			ExpiresAt: time.Date(2026, 7, 5, 23, 59, 59, 0, ny),
		},
		AppleURL:  "https://badges.example.com/passes/DGS-88231.pkpass?t=abc123",
		GoogleURL: "https://pay.google.com/gp/v/save/eyJhbGciOiJSUzI1NiJ9.e30.sig",
		BadgePNG:  []byte("\x89PNG\r\n\x1a\nFAKE-BADGE-BYTES"),
		PKPass:    []byte("PK\x03\x04FAKE-PKPASS-BYTES"),
		Location:  ny,
	}
}

// assertGolden compares got against testdata/<name>, rewriting it under -update.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run: go test ./internal/email -update): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s.\nIf the change is intentional, re-run with -update and review the diff.\n--- got ---\n%s", path, got)
	}
}

func TestRenderHTMLGolden(t *testing.T) {
	msg, err := email.Render(testData())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	assertGolden(t, "badge_day_pass.html", []byte(msg.HTML))
}

func TestRenderRedirectNoticeGolden(t *testing.T) {
	d := testData()
	d.Notice = "Delivery guard: redirect mode. This message would have been sent to casey@example.com."
	d.SubjectPrefix = "[would-send: casey@example.com] "
	msg, err := email.Render(d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	assertGolden(t, "badge_redirect.html", []byte(msg.HTML))
	if !strings.HasPrefix(msg.Subject, "[would-send: casey@example.com] ") {
		t.Errorf("Subject = %q, want redirect prefix", msg.Subject)
	}
}

func TestRenderSeasonMembershipGolden(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	d := testData()
	d.Badge.PassType = domain.PassTypeSeason
	d.Badge.Registration.RawPassType = "Season Membership"
	d.Badge.ExpiresAt = time.Date(2026, 12, 31, 23, 59, 59, 0, ny)
	msg, err := email.Render(d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	assertGolden(t, "badge_season.html", []byte(msg.HTML))
}

func TestRenderSubjectAndBodyContent(t *testing.T) {
	t.Parallel()
	msg, err := email.Render(testData())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "Your North Landing Community Day Pass — expires Jul 5, 2026"; msg.Subject != want {
		t.Errorf("Subject = %q, want %q", msg.Subject, want)
	}
	for _, want := range []string{
		"Add to Apple Wallet",
		"Save to Google Wallet",
		"Casey Chains",
		"Day Pass",
		"DGS-88231",
		"cid:" + email.BadgeCID,
		"Sun, Jul 5 2026 at 11:59 PM EDT",
	} {
		if !strings.Contains(msg.HTML, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	for _, want := range []string{"Add to Apple Wallet", "Save to Google Wallet", "Casey Chains", "DGS-88231"} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("text body missing %q", want)
		}
	}
}

func TestRenderHidesButtonsWhenLinksAbsent(t *testing.T) {
	t.Parallel()
	d := testData()
	d.AppleURL = ""
	d.GoogleURL = ""
	msg, err := email.Render(d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(msg.HTML, "Add to Apple Wallet") {
		t.Error("Apple button rendered without a link")
	}
	if strings.Contains(msg.HTML, "Save to Google Wallet") {
		t.Error("Google button rendered without a link")
	}
}

func TestRenderEscapesHostileNames(t *testing.T) {
	t.Parallel()
	d := testData()
	d.Badge.Registration.Name = `Casey <script>alert("x")</script>`
	msg, err := email.Render(d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(msg.HTML, "<script>") {
		t.Error("guest name was not HTML-escaped")
	}
}

func TestRenderValidates(t *testing.T) {
	t.Parallel()
	if _, err := email.Render(email.Data{}); err == nil {
		t.Error("expected error for empty data")
	}
	// A missing expiration is no longer an error: founder badges never expire.
	// See TestRenderNonExpiringBadge.
}

func TestMIMEStructure(t *testing.T) {
	t.Parallel()
	msg, err := email.Render(testData())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	raw, err := msg.MIME(email.MIMEOptions{
		From:      mail.Address{Name: "North Landing Community", Address: "club@gmail.com"},
		To:        []string{"casey@example.com"},
		Date:      time.Date(2026, 7, 4, 10, 5, 0, 0, time.UTC),
		MessageID: "test@northlanding",
		Boundary:  "nlbadge",
	})
	if err != nil {
		t.Fatalf("MIME: %v", err)
	}

	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("message does not parse: %v", err)
	}
	if got := parsed.Header.Get("From"); got != `"North Landing Community" <club@gmail.com>` {
		t.Errorf("From = %q", got)
	}
	if got := parsed.Header.Get("To"); got != "casey@example.com" {
		t.Errorf("To = %q", got)
	}
	if got := parsed.Header.Get("Message-ID"); got != "<test@northlanding>" {
		t.Errorf("Message-ID = %q", got)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	if subject != msg.Subject {
		t.Errorf("Subject = %q, want %q", subject, msg.Subject)
	}

	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	if mediaType != "multipart/mixed" {
		t.Fatalf("top-level type = %q, want multipart/mixed", mediaType)
	}

	var sawRelated, sawPKPass, sawInlineBadge, sawHTML, sawText bool
	mr := multipart.NewReader(parsed.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		ct := part.Header.Get("Content-Type")
		switch {
		case strings.HasPrefix(ct, "multipart/related"):
			sawRelated = true
			_, relParams, err := mime.ParseMediaType(ct)
			if err != nil {
				t.Fatalf("parse related: %v", err)
			}
			rr := multipart.NewReader(part, relParams["boundary"])
			for {
				rel, err := rr.NextPart()
				if err != nil {
					break
				}
				relCT := rel.Header.Get("Content-Type")
				switch {
				case strings.HasPrefix(relCT, "multipart/alternative"):
					_, altParams, err := mime.ParseMediaType(relCT)
					if err != nil {
						t.Fatalf("parse alternative: %v", err)
					}
					ar := multipart.NewReader(rel, altParams["boundary"])
					for {
						alt, err := ar.NextPart()
						if err != nil {
							break
						}
						switch {
						case strings.HasPrefix(alt.Header.Get("Content-Type"), "text/plain"):
							sawText = true
						case strings.HasPrefix(alt.Header.Get("Content-Type"), "text/html"):
							sawHTML = true
						}
					}
				case strings.HasPrefix(relCT, "image/png"):
					if rel.Header.Get("Content-ID") == "<"+email.BadgeCID+">" {
						sawInlineBadge = true
					}
				}
			}
		case strings.HasPrefix(ct, "application/vnd.apple.pkpass"):
			sawPKPass = true
			if !strings.Contains(part.Header.Get("Content-Disposition"), "attachment") {
				t.Error("pkpass is not an attachment")
			}
		}
	}

	if !sawRelated || !sawHTML || !sawText || !sawInlineBadge || !sawPKPass {
		t.Errorf("MIME parts missing: related=%v html=%v text=%v badge=%v pkpass=%v",
			sawRelated, sawHTML, sawText, sawInlineBadge, sawPKPass)
	}
}

func TestMIMEIsDeterministicWithFixedBoundary(t *testing.T) {
	t.Parallel()
	msg, err := email.Render(testData())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	opts := email.MIMEOptions{
		From:      mail.Address{Name: "North Landing Community", Address: "club@gmail.com"},
		To:        []string{"casey@example.com"},
		Date:      time.Date(2026, 7, 4, 10, 5, 0, 0, time.UTC),
		MessageID: "test@northlanding",
		Boundary:  "nlbadge",
	}
	first, err := msg.MIME(opts)
	if err != nil {
		t.Fatalf("MIME: %v", err)
	}
	second, err := msg.MIME(opts)
	if err != nil {
		t.Fatalf("MIME: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("MIME serialization is not deterministic")
	}
}

func TestMIMEWrapsLongLines(t *testing.T) {
	t.Parallel()
	msg, err := email.Render(testData())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	raw, err := msg.MIME(email.MIMEOptions{
		From:     mail.Address{Address: "club@gmail.com"},
		To:       []string{"casey@example.com"},
		Date:     time.Date(2026, 7, 4, 10, 5, 0, 0, time.UTC),
		Boundary: "nlbadge",
	})
	if err != nil {
		t.Fatalf("MIME: %v", err)
	}
	for i, line := range bytes.Split(raw, []byte("\r\n")) {
		if len(line) > 998 {
			t.Fatalf("line %d is %d octets, exceeding the RFC 5322 limit", i, len(line))
		}
	}
}

func TestMIMERequiresRecipients(t *testing.T) {
	t.Parallel()
	msg, err := email.Render(testData())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := msg.MIME(email.MIMEOptions{From: mail.Address{Address: "club@gmail.com"}}); err == nil {
		t.Error("expected error with no recipients")
	}
}

func TestRenderNonExpiringBadge(t *testing.T) {
	t.Parallel()
	msg, err := email.Render(email.Data{
		Badge: domain.Badge{
			Registration: domain.Registration{
				ID: "reg-1", Name: "A Founder", Email: "f@example.com",
				PurchasedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
			},
			PassType: domain.PassTypeFounder,
		},
		Location: time.UTC,
	})
	if err != nil {
		t.Fatalf("Render for a non-expiring badge = %v, want nil", err)
	}
	if !strings.Contains(msg.Text, "Never") {
		t.Errorf("plain-text body should say the badge never expires, got:\n%s", msg.Text)
	}
	if strings.Contains(msg.Subject, "expires") {
		t.Errorf("subject should not promise an expiry date for a founder badge, got %q", msg.Subject)
	}
}
