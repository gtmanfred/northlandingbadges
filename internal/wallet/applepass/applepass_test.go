package applepass_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/smallstep/pkcs7"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/testkeys"
	"github.com/northlanding/badges/internal/wallet/applepass"
)

func testConfig() config.AppleConfig {
	return config.AppleConfig{
		PassTypeIdentifier: "pass.com.northlanding.badge",
		TeamIdentifier:     "TESTTEAM01",
		OrganizationName:   "North Landing DGC",
		CertPEM:            testkeys.ApplePassCertPEM(),
		KeyPEM:             testkeys.ApplePassKeyPEM(),
		WWDRPEM:            testkeys.AppleWWDRPEM(),
	}
}

func testBadge() domain.Badge {
	ny, _ := time.LoadLocation("America/New_York")
	return domain.Badge{
		Registration: domain.Registration{
			ID:          "DGS-88231",
			Name:        "Casey Chains",
			Email:       "casey@example.com",
			RawPassType: "Season Membership",
			PurchasedAt: time.Date(2026, 4, 1, 9, 0, 0, 0, ny),
		},
		PassType:  domain.PassTypeSeason,
		ExpiresAt: time.Date(2026, 12, 31, 23, 59, 59, 0, ny),
	}
}

func readZip(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf(".pkpass is not a valid zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		out[f.Name] = b
	}
	return out
}

func TestBuildProducesValidZipWithRequiredEntries(t *testing.T) {
	t.Parallel()
	signer, err := applepass.NewSigner(testConfig())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	data, err := signer.Build(testBadge(), ny)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	files := readZip(t, data)
	for _, required := range []string{"pass.json", "manifest.json", "signature", "icon.png", "icon@2x.png", "logo.png"} {
		if len(files[required]) == 0 {
			t.Errorf("missing or empty %s in bundle", required)
		}
	}
}

func TestManifestHashesMatchEveryPayloadFile(t *testing.T) {
	t.Parallel()
	signer, err := applepass.NewSigner(testConfig())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	data, err := signer.Build(testBadge(), time.UTC)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	files := readZip(t, data)

	var manifest map[string]string
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatalf("manifest.json is not JSON: %v", err)
	}

	for name, data := range files {
		if name == "manifest.json" || name == "signature" {
			continue
		}
		want, ok := manifest[name]
		if !ok {
			t.Errorf("payload %s is not listed in manifest.json", name)
			continue
		}
		sum := sha1.Sum(data)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s digest = %s, manifest says %s", name, got, want)
		}
	}
	for name := range manifest {
		if _, ok := files[name]; !ok {
			t.Errorf("manifest lists %s which is not in the bundle", name)
		}
	}
}

func TestSignatureVerifiesAgainstTestCertificate(t *testing.T) {
	t.Parallel()
	signer, err := applepass.NewSigner(testConfig())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	data, err := signer.Build(testBadge(), time.UTC)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	files := readZip(t, data)

	p7, err := pkcs7.Parse(files["signature"])
	if err != nil {
		t.Fatalf("signature is not PKCS#7: %v", err)
	}
	if len(p7.Content) != 0 {
		t.Error("signature must be detached (no embedded content)")
	}
	// Detached signature: supply the manifest as the signed content.
	p7.Content = files["manifest.json"]
	if err := p7.Verify(); err != nil {
		t.Fatalf("signature does not verify against manifest.json: %v", err)
	}

	if signerCert := p7.GetOnlySigner(); signerCert == nil {
		t.Fatal("no signer certificate in signature")
	}

	// Tampering with the manifest must break verification.
	p7Tampered, err := pkcs7.Parse(files["signature"])
	if err != nil {
		t.Fatalf("pkcs7.Parse: %v", err)
	}
	p7Tampered.Content = append([]byte("x"), files["manifest.json"]...)
	if err := p7Tampered.Verify(); err == nil {
		t.Fatal("tampered manifest verified; signature is not binding")
	}
}

func TestPassJSONCarriesSpecFields(t *testing.T) {
	t.Parallel()
	signer, err := applepass.NewSigner(testConfig())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	b := testBadge()
	data, err := signer.Build(b, ny)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	files := readZip(t, data)

	var p struct {
		FormatVersion      int    `json:"formatVersion"`
		PassTypeIdentifier string `json:"passTypeIdentifier"`
		TeamIdentifier     string `json:"teamIdentifier"`
		SerialNumber       string `json:"serialNumber"`
		ExpirationDate     string `json:"expirationDate"`
		Description        string `json:"description"`
		Generic            struct {
			PrimaryFields   []struct{ Key, Label, Value string } `json:"primaryFields"`
			SecondaryFields []struct{ Key, Label, Value string } `json:"secondaryFields"`
		} `json:"generic"`
	}
	if err := json.Unmarshal(files["pass.json"], &p); err != nil {
		t.Fatalf("pass.json is not JSON: %v", err)
	}

	if p.FormatVersion != 1 {
		t.Errorf("formatVersion = %d", p.FormatVersion)
	}
	if p.PassTypeIdentifier != "pass.com.northlanding.badge" {
		t.Errorf("passTypeIdentifier = %q", p.PassTypeIdentifier)
	}
	if p.TeamIdentifier != "TESTTEAM01" {
		t.Errorf("teamIdentifier = %q", p.TeamIdentifier)
	}
	if p.SerialNumber != b.Registration.ID {
		t.Errorf("serialNumber = %q, want the registration ID %q", p.SerialNumber, b.Registration.ID)
	}
	if p.Description == "" {
		t.Error("description is required by Apple")
	}
	want := b.ExpiresAt.In(ny).Format(time.RFC3339)
	if p.ExpirationDate != want {
		t.Errorf("expirationDate = %q, want ISO 8601 %q", p.ExpirationDate, want)
	}
	if len(p.Generic.PrimaryFields) != 1 || p.Generic.PrimaryFields[0].Value != b.Registration.Name {
		t.Errorf("primaryFields = %+v, want guest name", p.Generic.PrimaryFields)
	}
	if len(p.Generic.SecondaryFields) != 1 || p.Generic.SecondaryFields[0].Value != "Season Member" {
		t.Errorf("secondaryFields = %+v, want pass type", p.Generic.SecondaryFields)
	}
}

func TestDayPassExpirationUsesClubLocalTime(t *testing.T) {
	t.Parallel()
	signer, err := applepass.NewSigner(testConfig())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	b := testBadge()
	b.PassType = domain.PassTypeDay
	b.ExpiresAt = time.Date(2026, 7, 5, 23, 59, 59, 0, ny)

	raw, err := signer.PassJSON(b, ny)
	if err != nil {
		t.Fatalf("PassJSON: %v", err)
	}
	var p struct {
		ExpirationDate string `json:"expirationDate"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ExpirationDate != "2026-07-05T23:59:59-04:00" {
		t.Errorf("expirationDate = %q, want club-local offset", p.ExpirationDate)
	}
}

func TestNewSignerRejectsBadInput(t *testing.T) {
	t.Parallel()
	if _, err := applepass.NewSigner(config.AppleConfig{}); err == nil {
		t.Error("expected error for unconfigured apple wallet")
	}

	bad := testConfig()
	bad.CertPEM = "not a pem"
	if _, err := applepass.NewSigner(bad); err == nil {
		t.Error("expected error for malformed certificate")
	}

	bad = testConfig()
	bad.KeyPEM = "-----BEGIN PRIVATE KEY-----\nbm9wZQ==\n-----END PRIVATE KEY-----\n"
	if _, err := applepass.NewSigner(bad); err == nil {
		t.Error("expected error for malformed key")
	}

	bad = testConfig()
	bad.WWDRPEM = "garbage"
	if _, err := applepass.NewSigner(bad); err == nil {
		t.Error("expected error for malformed WWDR cert")
	}
}

func TestBuildRejectsIncompleteBadge(t *testing.T) {
	t.Parallel()
	signer, err := applepass.NewSigner(testConfig())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if _, err := signer.Build(domain.Badge{}, time.UTC); err == nil {
		t.Error("expected error for empty badge")
	}
	noExpiry := testBadge()
	noExpiry.ExpiresAt = time.Time{}
	if _, err := signer.Build(noExpiry, time.UTC); err == nil {
		t.Error("expected error for badge without expiration")
	}
}

func TestSignerWorksWithoutWWDRChain(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.WWDRPEM = ""
	signer, err := applepass.NewSigner(cfg)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	data, err := signer.Build(testBadge(), time.UTC)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	files := readZip(t, data)
	p7, err := pkcs7.Parse(files["signature"])
	if err != nil {
		t.Fatalf("pkcs7.Parse: %v", err)
	}
	p7.Content = files["manifest.json"]
	if err := p7.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func founderBadge() domain.Badge {
	b := testBadge()
	b.PassType = domain.PassTypeFounder
	b.ExpiresAt = time.Time{}
	return b
}

func TestPassJSONOmitsExpirationForFounder(t *testing.T) {
	t.Parallel()
	signer, err := applepass.NewSigner(testConfig())
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	data, err := signer.PassJSON(founderBadge(), time.UTC)
	if err != nil {
		t.Fatalf("PassJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := decoded["expirationDate"]; ok {
		t.Error("pass.json contains expirationDate for a non-expiring badge, want it omitted")
	}
	if !bytes.Contains(data, []byte(`"Never"`)) {
		t.Error(`pass.json EXPIRES field should read "Never" for a non-expiring badge`)
	}
}
