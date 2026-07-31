// Package testkeys embeds the throwaway signing material used by tests.
//
// These are self-signed keys generated for this repository. They are NOT Apple
// or Google credentials and cannot produce a pass any real device will trust —
// their only job is to let signing and verification be exercised in CI with no
// secrets configured (spec §6).
package testkeys

import _ "embed"

//go:embed pass-cert.pem
var applePassCertPEM string

//go:embed pass-key.pem
var applePassKeyPEM string

//go:embed wwdr-cert.pem
var appleWWDRPEM string

//go:embed google-sa-key.pem
var googleKeyPEM string

// ApplePassCertPEM is the test pass-type-ID certificate.
func ApplePassCertPEM() string { return applePassCertPEM }

// ApplePassKeyPEM is the private key for ApplePassCertPEM.
func ApplePassKeyPEM() string { return applePassKeyPEM }

// AppleWWDRPEM is the test stand-in for Apple's WWDR intermediate.
func AppleWWDRPEM() string { return appleWWDRPEM }

// GoogleServiceAccountKeyPEM is the test RSA key used to sign Google Wallet JWTs.
func GoogleServiceAccountKeyPEM() string { return googleKeyPEM }
