// Package googlepass mints "Save to Google Wallet" JWTs.
//
// Google Wallet needs no signed bundle: the pass is described inline in a JWT
// signed with the issuer's service-account key, and the user saves it by opening
// pay.google.com/gp/v/save/<jwt>. RS256 signing is done with crypto/rsa, so there
// is no JWT dependency to keep current.
package googlepass

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/northlanding/badges/internal/badge"
	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/domain"
)

// SaveURLPrefix is where a save JWT is redeemed.
const SaveURLPrefix = "https://pay.google.com/gp/v/save/"

// LogoURI is where Google fetches the club mark. Google's servers pull this over
// HTTPS when the user saves the pass, so it must stay publicly reachable; a
// broken URL costs the pass its logo with no error reported back to us. Derived
// from badge.LogoAssetPath so moving the file cannot silently break rendering.
const LogoURI = "https://raw.githubusercontent.com/gtmanfred/northlandingbadges/main/" + badge.LogoAssetPath

// logoAltText is the accessibility description Google reads out for the mark.
const logoAltText = "North Landing Disc Golf Club logo"

// Issuer builds and signs save JWTs for one Google Wallet issuer account.
type Issuer struct {
	cfg   config.GoogleConfig
	key   *rsa.PrivateKey
	clock func() time.Time
}

// NewIssuer parses the service-account key and validates the issuer config.
func NewIssuer(cfg config.GoogleConfig) (*Issuer, error) {
	if !cfg.Configured() {
		return nil, errors.New("googlepass: google wallet is not configured")
	}
	key, err := parseRSAKey(cfg.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("googlepass: service account key: %w", err)
	}
	return &Issuer{cfg: cfg, key: key, clock: time.Now}, nil
}

// SaveLink returns the full URL to put behind the "Save to Google Wallet" button.
func (i *Issuer) SaveLink(b domain.Badge, loc *time.Location) (string, error) {
	jwt, err := i.SaveJWT(b, loc)
	if err != nil {
		return "", err
	}
	return SaveURLPrefix + jwt, nil
}

type localized struct {
	DefaultValue localizedValue `json:"defaultValue"`
}

type localizedValue struct {
	Language string `json:"language"`
	Value    string `json:"value"`
}

func text(v string) localized {
	return localized{DefaultValue: localizedValue{Language: "en-US", Value: v}}
}

type textModule struct {
	ID     string `json:"id"`
	Header string `json:"header"`
	Body   string `json:"body"`
}

type barcode struct {
	Type          string `json:"type"`
	Value         string `json:"value"`
	AlternateText string `json:"alternateText,omitempty"`
}

type dateTime struct {
	Date string `json:"date"`
}

type timeInterval struct {
	End dateTime `json:"end"`
}

type walletImage struct {
	SourceURI          imageURI  `json:"sourceUri"`
	ContentDescription localized `json:"contentDescription,omitempty"`
}

type imageURI struct {
	URI string `json:"uri"`
}

type genericObject struct {
	ID                 string        `json:"id"`
	ClassID            string        `json:"classId"`
	GenericType        string        `json:"genericType"`
	State              string        `json:"state"`
	CardTitle          localized     `json:"cardTitle"`
	Header             localized     `json:"header"`
	Logo               *walletImage  `json:"logo,omitempty"`
	HexBackgroundColor string        `json:"hexBackgroundColor"`
	TextModulesData    []textModule  `json:"textModulesData"`
	Barcode            *barcode      `json:"barcode,omitempty"`
	ValidTimeInterval  *timeInterval `json:"validTimeInterval,omitempty"`
}

type payload struct {
	GenericObjects []genericObject `json:"genericObjects"`
}

type claims struct {
	Issuer   string   `json:"iss"`
	Audience string   `json:"aud"`
	Type     string   `json:"typ"`
	IssuedAt int64    `json:"iat"`
	Origins  []string `json:"origins,omitempty"`
	Payload  payload  `json:"payload"`
}

// SaveJWT builds and signs the save JWT for b.
func (i *Issuer) SaveJWT(b domain.Badge, loc *time.Location) (string, error) {
	if err := b.Registration.Validate(); err != nil {
		return "", fmt.Errorf("googlepass: %w", err)
	}
	if loc == nil {
		loc = time.UTC
	}
	// The text module shows a human date; validTimeInterval must stay RFC3339
	// because Google parses it. Founder badges have neither.
	expiresText := "Never"
	var validInterval *timeInterval
	if b.Expires() {
		expiresText = b.ExpiresAt.In(loc).Format(badge.ShortDateLayout)
		validInterval = &timeInterval{End: dateTime{Date: b.ExpiresAt.In(loc).Format(time.RFC3339)}}
	}

	// The QR is a member convenience pointing at their PDGA page, so a
	// registrant with no PDGA number gets no barcode at all rather than one
	// that resolves to nothing.
	var bc *barcode
	if b.Registration.HasPDGA() {
		bc = &barcode{
			Type:          "QR_CODE",
			Value:         b.Registration.PDGAURL(),
			AlternateText: "PDGA #" + strings.TrimSpace(b.Registration.PDGANumber),
		}
	}

	obj := genericObject{
		ID:          fmt.Sprintf("%s.%s", i.cfg.IssuerID, sanitizeID(b.Registration.ID)),
		ClassID:     i.cfg.ClassID,
		GenericType: "GENERIC_TYPE_UNSPECIFIED",
		State:       "ACTIVE",
		CardTitle:   text("North Landing DGC"),
		Header:      text(b.PassType.Label()),
		Logo: &walletImage{
			SourceURI:          imageURI{URI: LogoURI},
			ContentDescription: text(logoAltText),
		},
		HexBackgroundColor: "#0b3d2e",
		TextModulesData: []textModule{
			{ID: "expires", Header: "Expiry Date", Body: expiresText},
			{ID: "member", Header: "Member", Body: b.Registration.Name},
			{ID: "registration", Header: "Registration", Body: b.Registration.ID},
		},
		Barcode:           bc,
		ValidTimeInterval: validInterval,
	}

	c := claims{
		Issuer:   i.cfg.ServiceAccountEmail,
		Audience: "google",
		Type:     "savetowallet",
		IssuedAt: i.now().Unix(),
		Payload:  payload{GenericObjects: []genericObject{obj}},
	}

	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("googlepass: marshal header: %w", err)
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("googlepass: marshal claims: %w", err)
	}

	signingInput := encode(header) + "." + encode(body)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("googlepass: sign jwt: %w", err)
	}
	return signingInput + "." + encode(sig), nil
}

// PublicKey exposes the signing key's public half so callers (and tests) can
// verify a minted JWT.
func (i *Issuer) PublicKey() *rsa.PublicKey { return &i.key.PublicKey }

func (i *Issuer) now() time.Time {
	if i.clock == nil {
		return time.Now()
	}
	return i.clock()
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// idUnsafe matches characters Google does not accept in an object ID.
var idUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func sanitizeID(raw string) string {
	return idUnsafe.ReplaceAllString(strings.TrimSpace(raw), "-")
}

func parseRSAKey(pemData string) (*rsa.PrivateKey, error) {
	rest := []byte(pemData)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no private key block found")
		}
		// A service-account PEM sometimes ships alongside its certificate; skip
		// anything that is not a key block.
		if strings.Contains(block.Type, "CERTIFICATE") {
			continue
		}
		if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			return key, nil
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("unsupported key block %q", block.Type)
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("key is %T, want RSA (Google Wallet requires RS256)", parsed)
		}
		return rsaKey, nil
	}
}

// Verify checks a save JWT's RS256 signature against pub and returns its raw
// claims JSON. Exported so both tests and the post-deploy smoke check can assert
// a minted link is well formed.
func Verify(jwt string, pub *rsa.PublicKey) ([]byte, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("googlepass: jwt has %d segments, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("googlepass: decode signature: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return nil, fmt.Errorf("googlepass: signature invalid: %w", err)
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("googlepass: decode claims: %w", err)
	}
	return body, nil
}
