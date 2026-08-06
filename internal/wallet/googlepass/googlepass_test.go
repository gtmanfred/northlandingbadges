package googlepass_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/northlanding/badges/internal/badge"
	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/testkeys"
	"github.com/northlanding/badges/internal/wallet/googlepass"
)

func testConfig() config.GoogleConfig {
	return config.GoogleConfig{
		IssuerID:            "3388000000012345678",
		ClassID:             "3388000000012345678.north-landing-badge",
		ServiceAccountEmail: "wallet@north-landing.iam.gserviceaccount.com",
		KeyPEM:              testkeys.GoogleServiceAccountKeyPEM(),
	}
}

func testBadge() domain.Badge {
	ny, _ := time.LoadLocation("America/New_York")
	return domain.Badge{
		Registration: domain.Registration{
			ID:          "DGS/88231",
			Name:        "Casey Chains",
			Email:       "casey@example.com",
			RawPassType: "Day Pass",
			PurchasedAt: time.Date(2026, 7, 4, 10, 0, 0, 0, ny),
		},
		PassType:  domain.PassTypeDay,
		ExpiresAt: time.Date(2026, 7, 5, 23, 59, 59, 0, ny),
	}
}

type parsedClaims struct {
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	Type     string `json:"typ"`
	IssuedAt int64  `json:"iat"`
	Payload  struct {
		GenericObjects []struct {
			ID          string `json:"id"`
			ClassID     string `json:"classId"`
			State       string `json:"state"`
			GenericType string `json:"genericType"`
			Header      struct {
				DefaultValue struct{ Value string } `json:"defaultValue"`
			} `json:"header"`
			Subheader struct {
				DefaultValue struct{ Value string } `json:"defaultValue"`
			} `json:"subheader"`
			TextModulesData []struct {
				ID, Header, Body string
			} `json:"textModulesData"`
			Barcode struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"barcode"`
			Logo struct {
				SourceURI struct {
					URI string `json:"uri"`
				} `json:"sourceUri"`
				ContentDescription struct {
					DefaultValue struct{ Value string } `json:"defaultValue"`
				} `json:"contentDescription"`
			} `json:"logo"`
			ValidTimeInterval struct {
				End struct {
					Date string `json:"date"`
				} `json:"end"`
			} `json:"validTimeInterval"`
		} `json:"genericObjects"`
	} `json:"payload"`
}

func TestSaveJWTVerifiesAndCarriesExpiration(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	b := testBadge()

	jwt, err := issuer.SaveJWT(b, ny)
	if err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	if got := len(strings.Split(jwt, ".")); got != 3 {
		t.Fatalf("jwt has %d segments, want 3", got)
	}

	raw, err := googlepass.Verify(jwt, issuer.PublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var c parsedClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("claims are not JSON: %v", err)
	}

	if c.Issuer != testConfig().ServiceAccountEmail {
		t.Errorf("iss = %q", c.Issuer)
	}
	if c.Audience != "google" {
		t.Errorf("aud = %q, want google", c.Audience)
	}
	if c.Type != "savetowallet" {
		t.Errorf("typ = %q, want savetowallet", c.Type)
	}
	if c.IssuedAt == 0 {
		t.Error("iat is unset")
	}
	if len(c.Payload.GenericObjects) != 1 {
		t.Fatalf("genericObjects = %d, want 1", len(c.Payload.GenericObjects))
	}

	obj := c.Payload.GenericObjects[0]
	wantExpiry := b.ExpiresAt.In(ny).Format(time.RFC3339)
	if obj.ValidTimeInterval.End.Date != wantExpiry {
		t.Errorf("validTimeInterval.end = %q, want %q", obj.ValidTimeInterval.End.Date, wantExpiry)
	}
	var expiresModule string
	for _, m := range obj.TextModulesData {
		if m.ID == "expires" {
			expiresModule = m.Body
		}
	}
	wantExpiryText := b.ExpiresAt.In(ny).Format(badge.ShortDateLayout)
	if expiresModule != wantExpiryText {
		t.Errorf("expires module = %q, want %q", expiresModule, wantExpiryText)
	}
	if obj.ClassID != testConfig().ClassID {
		t.Errorf("classId = %q", obj.ClassID)
	}
	if obj.Header.DefaultValue.Value != "Day Pass" {
		t.Errorf("header = %q, want the tier label", obj.Header.DefaultValue.Value)
	}
	var memberModule string
	for _, m := range obj.TextModulesData {
		if m.ID == "member" {
			memberModule = m.Body
		}
	}
	if memberModule != b.Registration.Name {
		t.Errorf("member module = %q, want guest name", memberModule)
	}
	if obj.Barcode.Value != b.Registration.ID {
		t.Errorf("barcode value = %q, want registration id", obj.Barcode.Value)
	}
}

func TestObjectIDIsSanitizedAndIssuerPrefixed(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	jwt, err := issuer.SaveJWT(testBadge(), time.UTC)
	if err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	raw, err := googlepass.Verify(jwt, issuer.PublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var c parsedClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := "3388000000012345678.DGS-88231" // slash replaced
	if got := c.Payload.GenericObjects[0].ID; got != want {
		t.Errorf("object id = %q, want %q", got, want)
	}
}

func TestSaveLinkUsesGooglePayPrefix(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	link, err := issuer.SaveLink(testBadge(), time.UTC)
	if err != nil {
		t.Fatalf("SaveLink: %v", err)
	}
	if !strings.HasPrefix(link, googlepass.SaveURLPrefix) {
		t.Errorf("link = %q, want %s prefix", link, googlepass.SaveURLPrefix)
	}
	if _, err := googlepass.Verify(strings.TrimPrefix(link, googlepass.SaveURLPrefix), issuer.PublicKey()); err != nil {
		t.Errorf("link JWT does not verify: %v", err)
	}
}

func TestVerifyRejectsTamperedJWT(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	jwt, err := issuer.SaveJWT(testBadge(), time.UTC)
	if err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	parts := strings.Split(jwt, ".")

	tampered := parts[0] + "." + parts[1] + "x." + parts[2]
	if _, err := googlepass.Verify(tampered, issuer.PublicKey()); err == nil {
		t.Error("tampered claims verified")
	}
	if _, err := googlepass.Verify("only.two", issuer.PublicKey()); err == nil {
		t.Error("malformed jwt verified")
	}
	if _, err := googlepass.Verify(parts[0]+"."+parts[1]+".!!!", issuer.PublicKey()); err == nil {
		t.Error("undecodable signature verified")
	}
}

func TestNewIssuerRejectsBadConfig(t *testing.T) {
	t.Parallel()
	if _, err := googlepass.NewIssuer(config.GoogleConfig{}); err == nil {
		t.Error("expected error for unconfigured issuer")
	}
	bad := testConfig()
	bad.KeyPEM = "not a key"
	if _, err := googlepass.NewIssuer(bad); err == nil {
		t.Error("expected error for malformed key")
	}
}

func TestNewIssuerSkipsCertificateBlocksInKeyPEM(t *testing.T) {
	t.Parallel()
	// A service-account PEM sometimes arrives with its certificate concatenated
	// ahead of the key; the key must still be found.
	cfg := testConfig()
	cfg.KeyPEM = testkeys.ApplePassCertPEM() + testkeys.GoogleServiceAccountKeyPEM()

	issuer, err := googlepass.NewIssuer(cfg)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	jwt, err := issuer.SaveJWT(testBadge(), time.UTC)
	if err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	if _, err := googlepass.Verify(jwt, issuer.PublicKey()); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestNewIssuerRejectsCertificateOnlyPEM(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.KeyPEM = testkeys.ApplePassCertPEM()
	if _, err := googlepass.NewIssuer(cfg); err == nil {
		t.Fatal("expected error when the PEM holds no private key")
	}
}

func TestSaveJWTRejectsIncompleteBadge(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	if _, err := issuer.SaveJWT(domain.Badge{}, time.UTC); err == nil {
		t.Error("expected error for empty badge")
	}
	// A missing expiration is no longer an error: founder badges never expire.
	// See TestSaveJWTAllowsNonExpiringBadge.
}

func TestSaveJWTAllowsNonExpiringBadge(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	b := testBadge()
	b.PassType = domain.PassTypeFounder
	b.ExpiresAt = time.Time{}

	jwt, err := issuer.SaveJWT(b, time.UTC)
	if err != nil {
		t.Fatalf("SaveJWT for a non-expiring badge = %v, want nil", err)
	}
	if jwt == "" {
		t.Fatal("SaveJWT returned an empty token")
	}
}

func TestLogoURIMatchesEmbeddedAssetPath(t *testing.T) {
	t.Parallel()
	if !strings.HasSuffix(googlepass.LogoURI, badge.LogoAssetPath) {
		t.Errorf("LogoURI %q does not end in the embedded asset path %q",
			googlepass.LogoURI, badge.LogoAssetPath)
	}
	if !strings.HasPrefix(googlepass.LogoURI, "https://raw.githubusercontent.com/gtmanfred/northlandingbadges/main/") {
		t.Errorf("LogoURI %q is not the expected public base", googlepass.LogoURI)
	}
}

func TestSaveJWTCarriesLogoURI(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	jwt, err := issuer.SaveJWT(testBadge(), time.UTC)
	if err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	body, err := googlepass.Verify(jwt, issuer.PublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var claims parsedClaims
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if len(claims.Payload.GenericObjects) != 1 {
		t.Fatalf("got %d generic objects, want 1", len(claims.Payload.GenericObjects))
	}
	obj := claims.Payload.GenericObjects[0]
	if obj.Logo.SourceURI.URI != googlepass.LogoURI {
		t.Errorf("logo uri = %q, want %q", obj.Logo.SourceURI.URI, googlepass.LogoURI)
	}
	if obj.Logo.ContentDescription.DefaultValue.Value == "" {
		t.Error("logo contentDescription is empty; alt text is required for accessibility")
	}
}

func TestSaveJWTOmitsValidTimeIntervalForFounder(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	b := testBadge()
	b.PassType = domain.PassTypeFounder
	b.ExpiresAt = time.Time{}

	jwt, err := issuer.SaveJWT(b, time.UTC)
	if err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	body, err := googlepass.Verify(jwt, issuer.PublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Inspect the raw object: an empty date string is invalid to Google, so the
	// key must be absent entirely rather than present-and-empty.
	var raw struct {
		Payload struct {
			GenericObjects []map[string]json.RawMessage `json:"genericObjects"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if len(raw.Payload.GenericObjects) != 1 {
		t.Fatalf("got %d generic objects, want 1", len(raw.Payload.GenericObjects))
	}
	if v, ok := raw.Payload.GenericObjects[0]["validTimeInterval"]; ok {
		t.Errorf("validTimeInterval present for a founder badge: %s", v)
	}
}

func TestSaveJWTKeepsValidTimeIntervalForExpiringBadge(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	jwt, err := issuer.SaveJWT(testBadge(), time.UTC)
	if err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	body, err := googlepass.Verify(jwt, issuer.PublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var claims parsedClaims
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if got := claims.Payload.GenericObjects[0].ValidTimeInterval.End.Date; got == "" {
		t.Error("expiring badge lost its validTimeInterval end date")
	}
}

func TestSaveJWTUsesLabelledReferenceLayout(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	b := testBadge()
	b.PassType = domain.PassTypeSeason
	b.ExpiresAt = time.Date(2026, 12, 31, 23, 59, 59, 0, ny)

	jwt, err := issuer.SaveJWT(b, ny)
	if err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	body, err := googlepass.Verify(jwt, issuer.PublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var claims parsedClaims
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	obj := claims.Payload.GenericObjects[0]

	if got := obj.Header.DefaultValue.Value; got != "Season Member" {
		t.Errorf("header = %q, want the tier label \"Season Member\"", got)
	}

	want := []struct{ id, header, body string }{
		{"expires", "Expiry Date", "Dec 31, 2026"},
		{"member", "Member", b.Registration.Name},
		{"registration", "Registration", b.Registration.ID},
	}
	if len(obj.TextModulesData) != len(want) {
		t.Fatalf("got %d text modules, want %d", len(obj.TextModulesData), len(want))
	}
	for i, w := range want {
		got := obj.TextModulesData[i]
		if got.ID != w.id || got.Header != w.header || got.Body != w.body {
			t.Errorf("module %d = {%q %q %q}, want {%q %q %q}",
				i, got.ID, got.Header, got.Body, w.id, w.header, w.body)
		}
	}

	// validTimeInterval stays machine-readable RFC3339, not display text.
	if got := obj.ValidTimeInterval.End.Date; got != "2026-12-31T23:59:59-05:00" {
		t.Errorf("validTimeInterval end = %q, want RFC3339", got)
	}
}

func TestSaveJWTDropsSubheader(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	jwt, err := issuer.SaveJWT(testBadge(), time.UTC)
	if err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	body, err := googlepass.Verify(jwt, issuer.PublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var raw struct {
		Payload struct {
			GenericObjects []map[string]json.RawMessage `json:"genericObjects"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := raw.Payload.GenericObjects[0]["subheader"]; ok {
		t.Errorf("subheader still present: %s", v)
	}
}

func TestSaveJWTShowsNeverForFounderExpiry(t *testing.T) {
	t.Parallel()
	issuer, err := googlepass.NewIssuer(testConfig())
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	b := testBadge()
	b.PassType = domain.PassTypeFounder
	b.ExpiresAt = time.Time{}

	jwt, err := issuer.SaveJWT(b, time.UTC)
	if err != nil {
		t.Fatalf("SaveJWT: %v", err)
	}
	body, err := googlepass.Verify(jwt, issuer.PublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var claims parsedClaims
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	obj := claims.Payload.GenericObjects[0]
	if got := obj.Header.DefaultValue.Value; got != "Founder" {
		t.Errorf("header = %q, want \"Founder\"", got)
	}
	if got := obj.TextModulesData[0].Body; got != "Never" {
		t.Errorf("expiry body = %q, want \"Never\"", got)
	}
}
