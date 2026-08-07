// Command mintpass writes a single signed .pkpass to disk so the Apple Wallet
// credentials can be verified on a real iPhone without running a poll cycle or
// sending mail.
//
// It reads the same APPLE_* variables the server does. With -testkeys it signs
// with the throwaway certificates in internal/testkeys instead: the bundle is
// structurally identical and useful for inspecting pass.json, but iOS will
// refuse to add it because the chain is not Apple's.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/northlanding/badges/internal/config"
	"github.com/northlanding/badges/internal/dgs"
	"github.com/northlanding/badges/internal/domain"
	"github.com/northlanding/badges/internal/expiry"
	"github.com/northlanding/badges/internal/testkeys"
	"github.com/northlanding/badges/internal/wallet/applepass"
)

func main() {
	var (
		name      = flag.String("name", "Test Registrant", "name shown on the pass")
		email     = flag.String("email", "test@example.com", "registrant email; seeds the serial number")
		pdga      = flag.String("pdga", "", "PDGA number; empty means no QR code")
		passType  = flag.String("type", "season_membership", "day_pass, season_membership, founder or sponsor")
		year      = flag.Int("year", 0, "season year; defaults to the current year")
		slug      = flag.String("slug", "mintpass", "event slug used to derive the serial number")
		out       = flag.String("out", "badge.pkpass", "output path")
		useTest   = flag.Bool("testkeys", false, "sign with throwaway test certs instead of APPLE_* env")
		printJSON = flag.Bool("print-json", false, "also print the unsigned pass.json")
	)
	flag.Parse()

	if err := run(*name, *email, *pdga, *passType, *slug, *out, *year, *useTest, *printJSON); err != nil {
		log.Fatalf("mintpass: %v", err)
	}
}

func run(name, email, pdga, passType, slug, out string, year int, useTest, printJSON bool) error {
	apple, err := appleConfig(useTest)
	if err != nil {
		return err
	}

	loc, err := time.LoadLocation(envOr("CLUB_TIMEZONE", config.DefaultTimezone))
	if err != nil {
		return fmt.Errorf("CLUB_TIMEZONE: %w", err)
	}

	purchasedAt := time.Now().In(loc)
	if year == 0 {
		year = purchasedAt.Year()
	}

	pt := domain.PassType(passType)
	if pt.Label() == passType && pt != domain.PassTypeDay {
		// Label() echoes the input for anything it does not recognise.
		return fmt.Errorf("unknown -type %q", passType)
	}

	expiresAt, err := expiry.Calculate(pt, purchasedAt, year, loc)
	if err != nil {
		return err
	}

	badge := domain.Badge{
		Registration: domain.Registration{
			ID:          dgs.RegistrationID(slug, email),
			Name:        name,
			Email:       email,
			RawPassType: pt.Label(),
			PurchasedAt: purchasedAt,
			SeasonYear:  year,
			PDGANumber:  pdga,
		},
		PassType:  pt,
		ExpiresAt: expiresAt,
	}

	signer, err := applepass.NewSigner(apple)
	if err != nil {
		return err
	}

	if printJSON {
		j, err := signer.PassJSON(badge, loc)
		if err != nil {
			return err
		}
		fmt.Println(string(j))
	}

	bundle, err := signer.Build(badge, loc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, bundle, 0o600); err != nil {
		return err
	}

	fmt.Printf("wrote %s (%d bytes)\n", out, len(bundle))
	fmt.Printf("  serial     %s\n", badge.Registration.ID)
	fmt.Printf("  pass type  %s / team %s\n", apple.PassTypeIdentifier, apple.TeamIdentifier)
	if badge.Expires() {
		fmt.Printf("  expires    %s\n", expiresAt.Format(time.RFC1123))
	} else {
		fmt.Printf("  expires    never (founder)\n")
	}
	if apple.WWDRPEM == "" {
		fmt.Println("  WARNING: no APPLE_WWDR_PEM, so the signature omits the intermediate; iOS will reject this pass")
	}
	if useTest {
		fmt.Println("  WARNING: signed with throwaway test certs; iOS will refuse to add it")
	}
	return nil
}

func appleConfig(useTest bool) (config.AppleConfig, error) {
	if useTest {
		return config.AppleConfig{
			PassTypeIdentifier: envOr("APPLE_PASS_TYPE_ID", "pass.com.northlanding.badge"),
			TeamIdentifier:     envOr("APPLE_TEAM_ID", "TESTTEAM01"),
			OrganizationName:   envOr("APPLE_ORG_NAME", "North Landing Community"),
			CertPEM:            testkeys.ApplePassCertPEM(),
			KeyPEM:             testkeys.ApplePassKeyPEM(),
			WWDRPEM:            testkeys.AppleWWDRPEM(),
		}, nil
	}

	cfg := config.AppleConfig{
		PassTypeIdentifier: os.Getenv("APPLE_PASS_TYPE_ID"),
		TeamIdentifier:     os.Getenv("APPLE_TEAM_ID"),
		OrganizationName:   envOr("APPLE_ORG_NAME", "North Landing Community"),
		CertPEM:            os.Getenv("APPLE_CERT_PEM"),
		KeyPEM:             os.Getenv("APPLE_KEY_PEM"),
		WWDRPEM:            os.Getenv("APPLE_WWDR_PEM"),
	}
	if !cfg.Configured() {
		return cfg, errors.New("APPLE_PASS_TYPE_ID, APPLE_TEAM_ID, APPLE_CERT_PEM and APPLE_KEY_PEM must be set (or pass -testkeys)")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
