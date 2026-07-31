// Package email renders the badge delivery email and serializes it to MIME.
//
// Rendering is pure: it takes a badge plus already-built wallet links and returns
// bytes. Nothing here touches the network, which is what lets the golden-file
// test in CI run with no credentials.
package email

import (
	"bytes"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/northlanding/badges/internal/badge"
	"github.com/northlanding/badges/internal/domain"
)

//go:embed templates/badge.html
var templatesFS embed.FS

var htmlTemplate = template.Must(template.ParseFS(templatesFS, "templates/badge.html"))

// BadgeCID is the Content-ID under which the badge PNG is inlined.
const BadgeCID = "badge.png"

// Data is everything the template needs.
type Data struct {
	Badge domain.Badge
	// AppleURL is the .pkpass download link; empty hides the Apple button.
	AppleURL string
	// GoogleURL is the Save-to-Google-Wallet link; empty hides that button.
	GoogleURL string
	// Notice is the delivery-guard banner text (redirect mode).
	Notice string
	// SubjectPrefix is prepended to the subject (redirect mode).
	SubjectPrefix string
	// BadgePNG is the rendered badge artwork, inlined and referenced by CID.
	BadgePNG []byte
	// PKPass is attached when Apple Wallet is configured.
	PKPass []byte
	// Location is the club timezone used to print the expiration.
	Location *time.Location
}

// Message is a rendered, transport-ready email.
type Message struct {
	Subject     string
	HTML        string
	Text        string
	Inline      []Part
	Attachments []Part
}

// Part is an inline image or file attachment.
type Part struct {
	Filename    string
	ContentType string
	ContentID   string
	Data        []byte
}

type templateData struct {
	Subject        string
	GuestName      string
	PassTypeLabel  string
	ExpiresText    string
	RegistrationID string
	AppleURL       string
	GoogleURL      string
	Notice         string
	BadgeCID       string
}

// Render builds the subject, HTML and plain-text bodies plus parts.
func Render(d Data) (Message, error) {
	if err := d.Badge.Registration.Validate(); err != nil {
		return Message{}, fmt.Errorf("email: %w", err)
	}
	loc := d.Location
	if loc == nil {
		loc = time.UTC
	}

	// Founder badges never expire, so the copy promises no date.
	expiresText := "Never"
	subject := fmt.Sprintf("%sYour North Landing DGC %s", d.SubjectPrefix, d.Badge.PassType.Label())
	if d.Badge.Expires() {
		expiresText = d.Badge.ExpiresAt.In(loc).Format(badge.DateLayout)
		subject = fmt.Sprintf("%sYour North Landing DGC %s — expires %s",
			d.SubjectPrefix, d.Badge.PassType.Label(), d.Badge.ExpiresAt.In(loc).Format("Jan 2, 2006"))
	}

	td := templateData{
		Subject:        subject,
		GuestName:      d.Badge.Registration.Name,
		PassTypeLabel:  d.Badge.PassType.Label(),
		ExpiresText:    expiresText,
		RegistrationID: d.Badge.Registration.ID,
		AppleURL:       d.AppleURL,
		GoogleURL:      d.GoogleURL,
		Notice:         d.Notice,
		BadgeCID:       BadgeCID,
	}

	var buf bytes.Buffer
	if err := htmlTemplate.ExecuteTemplate(&buf, "badge.html", td); err != nil {
		return Message{}, fmt.Errorf("email: render html: %w", err)
	}

	msg := Message{
		Subject: subject,
		HTML:    buf.String(),
		Text:    plainText(td),
	}
	if len(d.BadgePNG) > 0 {
		msg.Inline = append(msg.Inline, Part{
			Filename:    "badge.png",
			ContentType: "image/png",
			ContentID:   BadgeCID,
			Data:        d.BadgePNG,
		})
	}
	if len(d.PKPass) > 0 {
		msg.Attachments = append(msg.Attachments, Part{
			Filename:    fmt.Sprintf("north-landing-%s.pkpass", d.Badge.Registration.ID),
			ContentType: "application/vnd.apple.pkpass",
			Data:        d.PKPass,
		})
	}
	return msg, nil
}

func plainText(td templateData) string {
	var b strings.Builder
	if td.Notice != "" {
		fmt.Fprintf(&b, "%s\n\n", td.Notice)
	}
	fmt.Fprintf(&b, "NORTH LANDING DGC\n\nHi %s, your %s is ready.\n\n", td.GuestName, td.PassTypeLabel)
	fmt.Fprintf(&b, "Pass type:    %s\n", td.PassTypeLabel)
	fmt.Fprintf(&b, "Expires:      %s\n", td.ExpiresText)
	fmt.Fprintf(&b, "Registration: %s\n", td.RegistrationID)
	if td.AppleURL != "" {
		fmt.Fprintf(&b, "\nAdd to Apple Wallet: %s\n", td.AppleURL)
	}
	if td.GoogleURL != "" {
		fmt.Fprintf(&b, "Save to Google Wallet: %s\n", td.GoogleURL)
	}
	b.WriteString("\nThis badge is non-transferable and valid only through the expiration shown.\n")
	return b.String()
}

// MIMEOptions makes serialization deterministic, which the golden-file test in
// CI depends on.
type MIMEOptions struct {
	From      mail.Address
	To        []string
	Date      time.Time
	MessageID string
	// Boundary seeds the multipart boundaries; leave empty for random ones.
	Boundary string
}

// MIME serializes the message as a multipart/mixed RFC 5322 document.
func (m Message) MIME(opts MIMEOptions) ([]byte, error) {
	if len(opts.To) == 0 {
		return nil, errors.New("email: no recipients")
	}
	if opts.Date.IsZero() {
		opts.Date = time.Now()
	}

	var body bytes.Buffer
	mixed := multipart.NewWriter(&body)
	if opts.Boundary != "" {
		if err := mixed.SetBoundary(opts.Boundary + "-mixed"); err != nil {
			return nil, fmt.Errorf("email: boundary: %w", err)
		}
	}

	if err := m.writeBodyPart(mixed, opts.Boundary); err != nil {
		return nil, err
	}

	for _, att := range m.Attachments {
		if err := writePart(mixed, att, "attachment"); err != nil {
			return nil, err
		}
	}
	if err := mixed.Close(); err != nil {
		return nil, fmt.Errorf("email: close mixed: %w", err)
	}

	var out bytes.Buffer
	fmt.Fprintf(&out, "From: %s\r\n", opts.From.String())
	fmt.Fprintf(&out, "To: %s\r\n", strings.Join(opts.To, ", "))
	fmt.Fprintf(&out, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&out, "Date: %s\r\n", opts.Date.Format(time.RFC1123Z))
	if opts.MessageID != "" {
		fmt.Fprintf(&out, "Message-ID: <%s>\r\n", opts.MessageID)
	}
	out.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&out, "Content-Type: multipart/mixed; boundary=%q\r\n", mixed.Boundary())
	out.WriteString("\r\n")
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// writeBodyPart writes multipart/related{ multipart/alternative{text, html}, inline images }.
func (m Message) writeBodyPart(mixed *multipart.Writer, seed string) error {
	var relatedBuf bytes.Buffer
	related := multipart.NewWriter(&relatedBuf)
	if seed != "" {
		if err := related.SetBoundary(seed + "-related"); err != nil {
			return fmt.Errorf("email: boundary: %w", err)
		}
	}

	var altBuf bytes.Buffer
	alt := multipart.NewWriter(&altBuf)
	if seed != "" {
		if err := alt.SetBoundary(seed + "-alt"); err != nil {
			return fmt.Errorf("email: boundary: %w", err)
		}
	}
	if err := writeText(alt, "text/plain; charset=utf-8", m.Text); err != nil {
		return err
	}
	if err := writeText(alt, "text/html; charset=utf-8", m.HTML); err != nil {
		return err
	}
	if err := alt.Close(); err != nil {
		return fmt.Errorf("email: close alternative: %w", err)
	}

	altHeader := textproto.MIMEHeader{
		"Content-Type": {fmt.Sprintf("multipart/alternative; boundary=%q", alt.Boundary())},
	}
	w, err := related.CreatePart(altHeader)
	if err != nil {
		return fmt.Errorf("email: create alternative part: %w", err)
	}
	if _, err := w.Write(altBuf.Bytes()); err != nil {
		return fmt.Errorf("email: write alternative: %w", err)
	}

	for _, inline := range m.Inline {
		if err := writePart(related, inline, "inline"); err != nil {
			return err
		}
	}
	if err := related.Close(); err != nil {
		return fmt.Errorf("email: close related: %w", err)
	}

	relHeader := textproto.MIMEHeader{
		"Content-Type": {fmt.Sprintf("multipart/related; boundary=%q", related.Boundary())},
	}
	rw, err := mixed.CreatePart(relHeader)
	if err != nil {
		return fmt.Errorf("email: create related part: %w", err)
	}
	if _, err := rw.Write(relatedBuf.Bytes()); err != nil {
		return fmt.Errorf("email: write related: %w", err)
	}
	return nil
}

func writeText(w *multipart.Writer, contentType, content string) error {
	header := textproto.MIMEHeader{
		"Content-Type":              {contentType},
		"Content-Transfer-Encoding": {"quoted-printable"},
	}
	part, err := w.CreatePart(header)
	if err != nil {
		return fmt.Errorf("email: create text part: %w", err)
	}
	if _, err := part.Write([]byte(quotedPrintable(content))); err != nil {
		return fmt.Errorf("email: write text part: %w", err)
	}
	return nil
}

func writePart(w *multipart.Writer, p Part, disposition string) error {
	header := textproto.MIMEHeader{
		"Content-Type":              {fmt.Sprintf("%s; name=%q", p.ContentType, p.Filename)},
		"Content-Transfer-Encoding": {"base64"},
		"Content-Disposition":       {fmt.Sprintf("%s; filename=%q", disposition, p.Filename)},
	}
	if p.ContentID != "" {
		header.Set("Content-ID", "<"+p.ContentID+">")
	}
	part, err := w.CreatePart(header)
	if err != nil {
		return fmt.Errorf("email: create part %s: %w", p.Filename, err)
	}
	if _, err := part.Write(wrap(base64.StdEncoding.EncodeToString(p.Data), 76)); err != nil {
		return fmt.Errorf("email: write part %s: %w", p.Filename, err)
	}
	return nil
}

func wrap(s string, width int) []byte {
	var out bytes.Buffer
	for len(s) > width {
		out.WriteString(s[:width])
		out.WriteString("\r\n")
		s = s[width:]
	}
	out.WriteString(s)
	out.WriteString("\r\n")
	return out.Bytes()
}

// quotedPrintable encodes content conservatively: it escapes '=' and any byte
// outside printable ASCII, and hard-wraps long lines so no line exceeds the
// 998-octet RFC 5322 limit.
func quotedPrintable(s string) string {
	const maxLine = 74
	var out strings.Builder
	lineLen := 0
	writeAtom := func(atom string) {
		if lineLen+len(atom) > maxLine {
			out.WriteString("=\r\n")
			lineLen = 0
		}
		out.WriteString(atom)
		lineLen += len(atom)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\r':
			continue
		case c == '\n':
			out.WriteString("\r\n")
			lineLen = 0
		case c == '=' || c < 32 || c > 126:
			writeAtom(fmt.Sprintf("=%02X", c))
		default:
			writeAtom(string(c))
		}
	}
	return out.String()
}
