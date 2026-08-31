// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/license"
)

// The operator walkthrough is: mint a token with this command, paste it into
// LICENSE_TOKEN, restart, read the edition off the boot line. That only works
// if the minting side and the verifying side agree, so these tests mint through
// the same code path the operator uses and then load the result exactly as
// cmd/server does.
//
// Every key here is generated at test time. No private key is committed.

func testKeys(t *testing.T) (ed25519.PrivateKey, []string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return priv, []string{license.EncodeKey(pub)}
}

func communityFlags() claimFlags {
	return claimFlags{
		issuer: "FutureBuild, Inc.", subject: "acme-lumber", edition: "community",
		licensedVersion: "AI_LM 1.0.0", availability: "2026-01-15",
		independent: true, locations: 12,
		thresholdLocations: 50, thresholdRevenue: 1_000_000_000,
		productionUse: true,
	}
}

func TestMintedTokenLoadsAsTheEditionItClaims(t *testing.T) {
	priv, trust := testKeys(t)

	for _, ed := range []string{"evaluation", "community", "commercial"} {
		t.Run(ed, func(t *testing.T) {
			f := communityFlags()
			f.edition = ed
			claims, err := buildClaims(f)
			if err != nil {
				t.Fatalf("build claims: %v", err)
			}
			token, err := license.Mint(*claims, priv)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			lic, err := license.Load(license.Config{Token: token, PublicKeys: trust})
			if err != nil {
				t.Fatalf("the server refused a token this command minted: %v", err)
			}
			if string(lic.Edition()) != ed {
				t.Errorf("edition = %q, want %q", lic.Edition(), ed)
			}
			if lic.Subject() != "acme-lumber" {
				t.Errorf("subject = %q", lic.Subject())
			}
			if got, want := lic.ChangeDate().Format("2006-01-02"), "2031-01-15"; got != want {
				t.Errorf("change_date = %s, want %s", got, want)
			}
			// The determination's basis survives to the server, which is the
			// point of recording facts rather than computing them.
			c := lic.Claims()
			if c == nil || !c.IndependentOperator || c.LocationsAtIssue != 12 ||
				c.ThresholdLocations != 50 || c.ThresholdRevenueUSD != 1_000_000_000 {
				t.Errorf("the recorded basis did not reach the server: %+v", c)
			}
		})
	}
}

// TestMintedExpiredTokenBootsAsEvaluation is walkthrough step 6, executed.
func TestMintedExpiredTokenBootsAsEvaluation(t *testing.T) {
	priv, trust := testKeys(t)
	f := communityFlags()
	f.expiresAt = "2020-01-01T00:00:00Z"

	claims, err := buildClaims(f)
	if err != nil {
		t.Fatalf("build claims: %v", err)
	}
	token, err := license.Mint(*claims, priv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	lic, err := license.Load(license.Config{Token: token, PublicKeys: trust})
	if err != nil {
		t.Fatalf("an expired token must still boot: %v", err)
	}
	if lic.Edition() != license.EditionEvaluation {
		t.Errorf("edition = %q, want evaluation", lic.Edition())
	}
	if len(lic.Warnings()) == 0 || !strings.Contains(strings.Join(lic.Warnings(), " "), "2020-01-01") {
		t.Errorf("warnings must name the expiry date, got %v", lic.Warnings())
	}
}

// TestMintedConvertedTokenBootsAsAGPL is walkthrough step 7, executed.
func TestMintedConvertedTokenBootsAsAGPL(t *testing.T) {
	priv, trust := testKeys(t)
	f := communityFlags()
	f.availability = time.Now().AddDate(-6, 0, 0).Format("2006-01-02")

	claims, err := buildClaims(f)
	if err != nil {
		t.Fatalf("build claims: %v", err)
	}
	token, err := license.Mint(*claims, priv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	lic, err := license.Load(license.Config{Token: token, PublicKeys: trust})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if lic.Edition() != license.EditionAGPL {
		t.Errorf("edition = %q, want agpl — this version is past its Change Date", lic.Edition())
	}
}

// TestMintedCompetingUseTokenRefusesToBoot is walkthrough step 8, executed.
func TestMintedCompetingUseTokenRefusesToBoot(t *testing.T) {
	priv, trust := testKeys(t)
	f := communityFlags()
	f.competingUse = true

	claims, err := buildClaims(f)
	if err != nil {
		t.Fatalf("build claims: %v", err)
	}
	token, err := license.Mint(*claims, priv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := license.Load(license.Config{Token: token, PublicKeys: trust}); err == nil {
		t.Fatal("a competing-use token must refuse to start")
	} else if !strings.Contains(err.Error(), "Competing Use") {
		t.Errorf("the refusal must name the exclusion, got: %v", err)
	}
}

// TestCorruptedTokenRefusesToBoot is walkthrough step 5, executed: one
// character changed in the middle of a real token.
func TestCorruptedTokenRefusesToBoot(t *testing.T) {
	priv, trust := testKeys(t)
	claims, err := buildClaims(communityFlags())
	if err != nil {
		t.Fatalf("build claims: %v", err)
	}
	token, err := license.Mint(*claims, priv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	parts := strings.Split(token, ".")
	payload := []byte(parts[2])
	mid := len(payload) / 2
	if payload[mid] == 'A' {
		payload[mid] = 'B'
	} else {
		payload[mid] = 'A'
	}
	parts[2] = string(payload)

	if _, err := license.Load(license.Config{Token: strings.Join(parts, "."), PublicKeys: trust}); err == nil {
		t.Fatal("a corrupted token must refuse to start; it must not boot as evaluation")
	}
}

// TestBuildClaimsRefusesTokensTheServerWouldReject. Failing at the desk of the
// person issuing the licence is much cheaper than failing at a dealer's.
func TestBuildClaimsRefusesTokensTheServerWouldReject(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*claimFlags)
		want   string
	}{
		{"no subject", func(f *claimFlags) { f.subject = "" }, "-subject"},
		{"no licensed version", func(f *claimFlags) { f.licensedVersion = "" }, "-licensed-version"},
		{"no availability date", func(f *claimFlags) { f.availability = "" }, "-availability-date"},
		{"availability date is not a date", func(f *claimFlags) { f.availability = "last January" }, "YYYY-MM-DD"},
		{"agpl cannot be issued", func(f *claimFlags) { f.edition = "agpl" }, "cannot be issued"},
		{"unknown edition", func(f *claimFlags) { f.edition = "enterprise" }, "not one of"},
		{"expires-at is not RFC3339", func(f *claimFlags) { f.expiresAt = "tomorrow" }, "RFC3339"},
		{"issued-at is not RFC3339", func(f *claimFlags) { f.issuedAt = "yesterday" }, "RFC3339"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := communityFlags()
			tc.mutate(&f)
			_, err := buildClaims(f)
			if err == nil {
				t.Fatal("expected keygen to refuse these flags")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestExpiresInIsRelativeToIssue(t *testing.T) {
	f := communityFlags()
	f.issuedAt = "2026-02-01T00:00:00Z"
	f.expiresIn = 24 * time.Hour

	claims, err := buildClaims(f)
	if err != nil {
		t.Fatalf("build claims: %v", err)
	}
	if claims.ExpiresAt == nil {
		t.Fatal("expected an expiry")
	}
	if got, want := claims.ExpiresAt.Format(time.RFC3339), "2026-02-02T00:00:00Z"; got != want {
		t.Errorf("expires_at = %s, want %s", got, want)
	}
}

func TestNoExpiryByDefault(t *testing.T) {
	claims, err := buildClaims(communityFlags())
	if err != nil {
		t.Fatalf("build claims: %v", err)
	}
	if claims.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil — a perpetual licence has no expiry", claims.ExpiresAt)
	}
}

func TestResolveKeyGeneratesWhenNoneGiven(t *testing.T) {
	priv, pub, generated, err := resolveKey("")
	if err != nil {
		t.Fatalf("resolve key: %v", err)
	}
	if !generated {
		t.Error("expected a throwaway keypair to be generated")
	}
	if !priv.Public().(ed25519.PublicKey).Equal(pub) {
		t.Error("the printed public key does not belong to the printed private key")
	}

	priv2, pub2, generated2, err := resolveKey(license.EncodeKey(priv))
	if err != nil {
		t.Fatalf("resolve supplied key: %v", err)
	}
	if generated2 {
		t.Error("a supplied key must not be replaced by a generated one")
	}
	if !priv2.Equal(priv) || !pub2.Equal(pub) {
		t.Error("a supplied key did not round-trip")
	}
	if _, _, _, err := resolveKey("not-a-key"); err == nil {
		t.Error("expected an unparseable -key to be refused")
	}
}
