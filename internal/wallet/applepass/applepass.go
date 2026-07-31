// Package applepass builds and signs Apple Wallet .pkpass bundles.
//
// A .pkpass is a zip containing pass.json, the referenced images, a
// manifest.json of SHA-1 digests, and a detached PKCS#7 signature over that
// manifest. Everything here is done with the standard library plus a PKCS#7
// implementation — no `openssl` shell-out, so it runs in a scratch container.
package applepass

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/smallstep/pkcs7"

	"github.com/northlanding/badges/internal/badge"
	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/domain"
)

// Description is the accessibility description Apple requires in pass.json.
const Description = "North Landing DGC membership badge"

// Signer turns badges into signed .pkpass bundles.
type Signer struct {
	cert  *x509.Certificate
	key   crypto.PrivateKey
	wwdr  *x509.Certificate
	cfg   config.AppleConfig
	clock func() time.Time
}

// NewSigner parses the configured certificate material.
func NewSigner(cfg config.AppleConfig) (*Signer, error) {
	if !cfg.Configured() {
		return nil, errors.New("applepass: apple wallet is not configured")
	}
	cert, key, err := parseKeyPair(cfg.CertPEM, cfg.KeyPEM)
	if err != nil {
		return nil, err
	}
	s := &Signer{cert: cert, key: key, cfg: cfg, clock: time.Now}

	if cfg.WWDRPEM != "" {
		wwdr, err := parseCertificate(cfg.WWDRPEM)
		if err != nil {
			return nil, fmt.Errorf("applepass: WWDR certificate: %w", err)
		}
		s.wwdr = wwdr
	}
	return s, nil
}

// Build renders, packages and signs the .pkpass for b.
func (s *Signer) Build(b domain.Badge, loc *time.Location) ([]byte, error) {
	if err := b.Registration.Validate(); err != nil {
		return nil, fmt.Errorf("applepass: %w", err)
	}
	if b.ExpiresAt.IsZero() {
		return nil, errors.New("applepass: badge has no expiration")
	}
	if loc == nil {
		loc = time.UTC
	}

	passJSON, err := s.passJSON(b, loc)
	if err != nil {
		return nil, err
	}
	files, err := assets()
	if err != nil {
		return nil, err
	}
	files["pass.json"] = passJSON

	manifest, err := manifestJSON(files)
	if err != nil {
		return nil, err
	}
	files["manifest.json"] = manifest

	signature, err := s.sign(manifest)
	if err != nil {
		return nil, err
	}
	files["signature"] = signature

	return zipFiles(files)
}

// PassJSON exposes the unsigned pass.json payload, for inspection and tests.
func (s *Signer) PassJSON(b domain.Badge, loc *time.Location) ([]byte, error) {
	return s.passJSON(b, loc)
}

type field struct {
	Key       string `json:"key"`
	Label     string `json:"label,omitempty"`
	Value     string `json:"value"`
	DateStyle string `json:"dateStyle,omitempty"`
	TimeStyle string `json:"timeStyle,omitempty"`
}

type barcode struct {
	Format          string `json:"format"`
	Message         string `json:"message"`
	MessageEncoding string `json:"messageEncoding"`
}

type structure struct {
	PrimaryFields   []field `json:"primaryFields"`
	SecondaryFields []field `json:"secondaryFields"`
	AuxiliaryFields []field `json:"auxiliaryFields,omitempty"`
	BackFields      []field `json:"backFields,omitempty"`
}

type pass struct {
	FormatVersion      int       `json:"formatVersion"`
	PassTypeIdentifier string    `json:"passTypeIdentifier"`
	TeamIdentifier     string    `json:"teamIdentifier"`
	OrganizationName   string    `json:"organizationName"`
	Description        string    `json:"description"`
	SerialNumber       string    `json:"serialNumber"`
	ExpirationDate     string    `json:"expirationDate,omitempty"`
	LogoText           string    `json:"logoText"`
	ForegroundColor    string    `json:"foregroundColor"`
	BackgroundColor    string    `json:"backgroundColor"`
	LabelColor         string    `json:"labelColor"`
	Barcodes           []barcode `json:"barcodes"`
	Generic            structure `json:"generic"`
}

func (s *Signer) passJSON(b domain.Badge, loc *time.Location) ([]byte, error) {
	if loc == nil {
		loc = time.UTC
	}
	// A founder badge never expires: omit expirationDate entirely rather than
	// formatting a zero time, and say so in the fields Apple renders.
	expirationDate := ""
	expiresField := field{Key: "expires", Label: "EXPIRES", Value: "Never"}
	terms := "Non-transferable. Valid for as long as the club recognises this badge."
	if b.Expires() {
		expirationDate = b.ExpiresAt.In(loc).Format(time.RFC3339)
		expiresField = field{
			Key:       "expires",
			Label:     "EXPIRES",
			Value:     expirationDate,
			DateStyle: "PKDateStyleMedium",
			TimeStyle: "PKDateStyleShort",
		}
		terms = "Non-transferable. Valid only through the expiration shown."
	}

	p := pass{
		FormatVersion:      1,
		PassTypeIdentifier: s.cfg.PassTypeIdentifier,
		TeamIdentifier:     s.cfg.TeamIdentifier,
		OrganizationName:   s.cfg.OrganizationName,
		Description:        Description,
		SerialNumber:       b.Registration.ID,
		ExpirationDate:     expirationDate,
		LogoText:           "North Landing DGC",
		ForegroundColor:    "rgb(255,255,255)",
		BackgroundColor:    "rgb(11,61,46)",
		LabelColor:         "rgb(232,180,58)",
		Barcodes: []barcode{{
			Format:          "PKBarcodeFormatQR",
			Message:         b.Registration.ID,
			MessageEncoding: "iso-8859-1",
		}},
		Generic: structure{
			PrimaryFields: []field{
				{Key: "guest", Label: "GUEST", Value: b.Registration.Name},
			},
			SecondaryFields: []field{
				{Key: "passType", Label: "PASS TYPE", Value: b.PassType.Label()},
			},
			AuxiliaryFields: []field{expiresField},
			BackFields: []field{
				{Key: "registration", Label: "REGISTRATION", Value: b.Registration.ID},
				{Key: "terms", Label: "TERMS", Value: terms},
			},
		},
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("applepass: marshal pass.json: %w", err)
	}
	return data, nil
}

// assets renders the images every .pkpass must carry.
func assets() (map[string][]byte, error) {
	out := map[string][]byte{}
	icons := map[string]int{"icon.png": 29, "icon@2x.png": 58, "icon@3x.png": 87}
	for name, size := range icons {
		data, err := badge.Icon(size)
		if err != nil {
			return nil, fmt.Errorf("applepass: %s: %w", name, err)
		}
		out[name] = data
	}
	logos := map[string][2]int{"logo.png": {160, 50}, "logo@2x.png": {320, 100}}
	for name, dims := range logos {
		data, err := badge.Logo(dims[0], dims[1])
		if err != nil {
			return nil, fmt.Errorf("applepass: %s: %w", name, err)
		}
		out[name] = data
	}
	return out, nil
}

// manifestJSON maps each payload file to its SHA-1, which is what Apple's
// verifier expects (SHA-1 is Apple's requirement here, not a security choice of
// ours; integrity comes from the PKCS#7 signature over this manifest).
func manifestJSON(files map[string][]byte) ([]byte, error) {
	digests := make(map[string]string, len(files))
	for name, data := range files {
		sum := sha1.Sum(data)
		digests[name] = hex.EncodeToString(sum[:])
	}
	data, err := json.MarshalIndent(digests, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("applepass: marshal manifest.json: %w", err)
	}
	return data, nil
}

func (s *Signer) sign(manifest []byte) ([]byte, error) {
	signed, err := pkcs7.NewSignedData(manifest)
	if err != nil {
		return nil, fmt.Errorf("applepass: new signed data: %w", err)
	}
	signed.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)

	var chain []*x509.Certificate
	if s.wwdr != nil {
		chain = append(chain, s.wwdr)
	}
	if err := signed.AddSignerChain(s.cert, s.key, chain, pkcs7.SignerInfoConfig{}); err != nil {
		return nil, fmt.Errorf("applepass: add signer: %w", err)
	}
	// Apple expects a detached signature: the manifest itself is shipped in the
	// zip, not embedded in the PKCS#7 blob.
	signed.Detach()

	der, err := signed.Finish()
	if err != nil {
		return nil, fmt.Errorf("applepass: finish signature: %w", err)
	}
	return der, nil
}

// zipFiles writes files in sorted order so bundles are byte-stable apart from
// the signature, which carries its own randomness.
func zipFiles(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			return nil, fmt.Errorf("applepass: zip create %s: %w", name, err)
		}
		if _, err := w.Write(files[name]); err != nil {
			return nil, fmt.Errorf("applepass: zip write %s: %w", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("applepass: zip close: %w", err)
	}
	return buf.Bytes(), nil
}

func parseKeyPair(certPEM, keyPEM string) (*x509.Certificate, crypto.PrivateKey, error) {
	cert, err := parseCertificate(certPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("applepass: pass certificate: %w", err)
	}
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("applepass: pass key: %w", err)
	}
	return cert, key, nil
}

func parseCertificate(pemData string) (*x509.Certificate, error) {
	for rest := []byte(pemData); len(rest) > 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		return cert, nil
	}
	return nil, errors.New("no CERTIFICATE block found")
}

func parsePrivateKey(pemData string) (crypto.PrivateKey, error) {
	for rest := []byte(pemData); len(rest) > 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			continue
		}
		if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			return key, nil
		}
		if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			return key, nil
		}
		if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
			return key, nil
		}
		return nil, fmt.Errorf("unsupported private key block %q", block.Type)
	}
	return nil, errors.New("no private key block found")
}
