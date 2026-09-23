// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Command keygen mints AI_LM licence tokens and the Ed25519 keypairs that sign
// them (internal/license).
//
// It exists so that no private key is committed to this repository. The test
// suite generates its own keypair at test time and calls license.Mint directly;
// this command is the operator-facing path to the same function, for the
// walkthrough and for issuing a real token once the signing key has an owner
// and a storage location.
//
//	# A keypair. The private key is a secret: it is the authority to issue
//	# licences for every AI_LM deployment that trusts it.
//	go run ./cmd/keygen -genkey
//
//	# A community token for a twelve-branch dealer, expiring in a year.
//	go run ./cmd/keygen -key "$LICENSE_PRIVATE_KEY" \
//	    -subject acme-lumber -edition community \
//	    -licensed-version "AI_LM 1.0.0" -availability-date 2026-01-15 \
//	    -independent-operator -locations 12 -production-use -expires-in 8760h
//
//	# Then, for the service:
//	export LICENSE_PUBLIC_KEY=...   # printed by -genkey
//	export LICENSE_TOKEN=...        # printed above
//
// With no -key, a throwaway keypair is generated and printed alongside the
// token, which is what the operator walkthrough uses.
//
// WHAT THE CLAIMS MEAN: the token RECORDS a determination, it does not COMPUTE
// one. Whether a dealer is an Independent Operator, and whether a use is
// Competing Use, are legal judgements about corporate control and intent —
// made by a human, before this command is run. The flags below are how that
// judgement, and the facts it rested on, get written down and signed.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/license"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		genKeyOnly = flag.Bool("genkey", false, "generate and print a keypair, then exit without minting a token")
		privB64    = flag.String("key", "", "base64 Ed25519 private key to sign with (default: generate a throwaway keypair and print it)")

		issuer          = flag.String("issuer", "FutureBuild, Inc.", "who made this determination")
		subject         = flag.String("subject", "", "the deployment this determination is about (required)")
		edition         = flag.String("edition", "community", "evaluation | community | commercial (agpl is derived from the Change Date, never issued)")
		licensedVersion = flag.String("licensed-version", "", `the Licensed Work, e.g. "AI_LM 1.0.0" (required)`)
		availability    = flag.String("availability-date", "", "YYYY-MM-DD the licensed version was first made available; the Change Date is this plus five years (required)")

		independent = flag.Bool("independent-operator", false, "the size determination AS MADE: is this dealer a Community Member?")
		locations   = flag.Int("locations", 0, "Locations operated, on the day of issue — the fact the size determination rested on")
		revenueOver = flag.Bool("revenue-over-1b", false, "controls, or is controlled by, an entity with revenue over the threshold")

		thresholdLocations = flag.Int("threshold-locations", 50, "the Locations threshold IN FORCE AT ISSUE (recorded, not compiled in: the Standard still marks it [COUNSEL:])")
		thresholdRevenue   = flag.Int64("threshold-revenue-usd", 1_000_000_000, "the revenue threshold IN FORCE AT ISSUE, in USD")

		productionUse = flag.Bool("production-use", false, "licensed for a Production Purpose (as opposed to evaluation only)")
		competingUse  = flag.Bool("competing-use-acknowledged", false, "DANGER: records that this use is Competing Use, which is outside the grant. A service given such a token REFUSES TO START. Present only so that refusal can be exercised")

		expiresIn = flag.Duration("expires-in", 0, "expiry as a duration from now (e.g. 8760h); zero means no expiry")
		expiresAt = flag.String("expires-at", "", "expiry as an RFC3339 instant; overrides -expires-in. Use a past instant to exercise the expired-token path")
		issuedAt  = flag.String("issued-at", "", "RFC3339 instant of issue (default: now)")
		features  = flag.String("features", "", "comma-separated feature list recorded on the token (nothing consults it: v1 gates nothing)")

		asJSON = flag.Bool("json", false, "print the token, keys and claims as JSON")
	)
	flag.Parse()

	priv, pub, generated, err := resolveKey(*privB64)
	if err != nil {
		return err
	}

	if *genKeyOnly {
		printKeys(priv, pub)
		return nil
	}

	claims, err := buildClaims(claimFlags{
		issuer: *issuer, subject: *subject, edition: *edition,
		licensedVersion: *licensedVersion, availability: *availability,
		independent: *independent, locations: *locations, revenueOver: *revenueOver,
		thresholdLocations: *thresholdLocations, thresholdRevenue: *thresholdRevenue,
		productionUse: *productionUse, competingUse: *competingUse,
		expiresIn: *expiresIn, expiresAt: *expiresAt, issuedAt: *issuedAt,
		features: *features,
	})
	if err != nil {
		return err
	}

	token, err := license.Mint(*claims, priv)
	if err != nil {
		return err
	}

	if *asJSON {
		out := map[string]any{
			"token":      token,
			"public_key": license.EncodeKey(pub),
			"key_id":     license.KeyID(pub),
			"claims":     claims,
		}
		if generated {
			out["private_key"] = license.EncodeKey(priv)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if generated {
		fmt.Println("# No -key given, so a throwaway keypair was generated. Keep the private key")
		fmt.Println("# out of the repository and out of version control.")
		printKeys(priv, pub)
		fmt.Println()
	}
	fmt.Printf("LICENSE_PUBLIC_KEY=%s\n", license.EncodeKey(pub))
	fmt.Printf("LICENSE_TOKEN=%s\n", token)
	if claims.CompetingUseAcknowledged {
		fmt.Println()
		fmt.Println("# WARNING: competing_use_acknowledged is set. A service given this token will")
		fmt.Println("# REFUSE TO START, naming the field-of-use exclusion. That is the intended")
		fmt.Println("# behaviour: Competing Use is outside the grant for everyone regardless of size.")
	}
	return nil
}

func resolveKey(privB64 string) (priv ed25519.PrivateKey, pub ed25519.PublicKey, generated bool, err error) {
	if strings.TrimSpace(privB64) != "" {
		priv, err = license.ParsePrivateKey(privB64)
		if err != nil {
			return nil, nil, false, err
		}
		return priv, priv.Public().(ed25519.PublicKey), false, nil
	}
	pub, priv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, false, fmt.Errorf("generate keypair: %w", err)
	}
	return priv, pub, true, nil
}

func printKeys(priv ed25519.PrivateKey, pub ed25519.PublicKey) {
	fmt.Printf("LICENSE_PRIVATE_KEY=%s\n", license.EncodeKey(priv))
	fmt.Printf("LICENSE_PUBLIC_KEY=%s\n", license.EncodeKey(pub))
	fmt.Printf("# key id: %s\n", license.KeyID(pub))
}

type claimFlags struct {
	issuer, subject, edition      string
	licensedVersion, availability string
	independent                   bool
	locations                     int
	revenueOver                   bool
	thresholdLocations            int
	thresholdRevenue              int64
	productionUse, competingUse   bool
	expiresIn                     time.Duration
	expiresAt, issuedAt, features string
}

// buildClaims turns the flags into a claim set, refusing the ones that would
// mint a token the server will reject at boot. Failing here, at the desk of the
// person issuing the licence, is much cheaper than failing at a dealer's.
func buildClaims(f claimFlags) (*license.Claims, error) {
	if strings.TrimSpace(f.subject) == "" {
		return nil, fmt.Errorf("-subject is required: name the deployment this determination is about")
	}
	if strings.TrimSpace(f.licensedVersion) == "" {
		return nil, fmt.Errorf(`-licensed-version is required, e.g. "AI_LM 1.0.0"`)
	}
	if strings.TrimSpace(f.availability) == "" {
		return nil, fmt.Errorf("-availability-date is required: conversion to the Change License is per-version, " +
			"so a token that cannot say when its version became available cannot say when it converts")
	}
	if _, err := time.Parse("2006-01-02", f.availability); err != nil {
		return nil, fmt.Errorf("-availability-date %q is not YYYY-MM-DD", f.availability)
	}
	switch license.Edition(f.edition) {
	case license.EditionEvaluation, license.EditionCommunity, license.EditionCommercial:
	case license.EditionAGPL:
		return nil, fmt.Errorf("-edition agpl cannot be issued: conversion is derived from the availability date (§4), " +
			"so set -availability-date more than five years ago instead")
	default:
		return nil, fmt.Errorf("-edition %q is not one of evaluation, community, commercial", f.edition)
	}

	issued := time.Now().UTC()
	if strings.TrimSpace(f.issuedAt) != "" {
		var err error
		if issued, err = time.Parse(time.RFC3339, f.issuedAt); err != nil {
			return nil, fmt.Errorf("-issued-at %q is not RFC3339: %w", f.issuedAt, err)
		}
	}

	var expires *time.Time
	switch {
	case strings.TrimSpace(f.expiresAt) != "":
		exp, err := time.Parse(time.RFC3339, f.expiresAt)
		if err != nil {
			return nil, fmt.Errorf("-expires-at %q is not RFC3339: %w", f.expiresAt, err)
		}
		exp = exp.UTC()
		expires = &exp
	case f.expiresIn > 0:
		exp := issued.Add(f.expiresIn).UTC()
		expires = &exp
	}

	var featureList []string
	for _, x := range strings.Split(f.features, ",") {
		if x = strings.TrimSpace(x); x != "" {
			featureList = append(featureList, x)
		}
	}

	return &license.Claims{
		Issuer:                   f.issuer,
		Subject:                  f.subject,
		Edition:                  license.Edition(f.edition),
		LicensedVersion:          f.licensedVersion,
		AvailabilityDate:         f.availability,
		IndependentOperator:      f.independent,
		LocationsAtIssue:         f.locations,
		ControlledRevenueOver1B:  f.revenueOver,
		ThresholdLocations:       f.thresholdLocations,
		ThresholdRevenueUSD:      f.thresholdRevenue,
		ProductionUse:            f.productionUse,
		CompetingUseAcknowledged: f.competingUse,
		IssuedAt:                 issued.UTC(),
		ExpiresAt:                expires,
		Features:                 featureList,
	}, nil
}
