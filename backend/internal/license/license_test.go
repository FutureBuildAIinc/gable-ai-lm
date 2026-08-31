// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- test fixtures -----------------------------------------------------------
//
// Every key in this suite is generated HERE, at test time. No private key is
// committed to this repository, and none needs to be: Mint is exported for
// exactly this reason (and for cmd/keygen, which is the operator-facing path to
// the same function).

type signer struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return signer{pub: pub, priv: priv}
}

func (s signer) trust() []string { return []string{EncodeKey(s.pub)} }

func (s signer) mint(t *testing.T, c Claims) string {
	t.Helper()
	tok, err := Mint(c, s.priv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

func ptr(t time.Time) *time.Time { return &t }

// communityClaims is a well-formed Independent Operator token: twelve
// locations, no billion-dollar parent, in production, under the thresholds in
// force on the day it was issued.
func communityClaims() Claims {
	return Claims{
		Issuer:                   "FutureBuild, Inc.",
		Subject:                  "acme-lumber",
		Edition:                  EditionCommunity,
		LicensedVersion:          "AI_LM 1.0.0",
		AvailabilityDate:         "2026-01-15",
		IndependentOperator:      true,
		LocationsAtIssue:         12,
		ControlledRevenueOver1B:  false,
		ThresholdLocations:       50,
		ThresholdRevenueUSD:      1_000_000_000,
		ProductionUse:            true,
		CompetingUseAcknowledged: false,
		IssuedAt:                 time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Features:                 []string{"ai_briefing", "ors_routing"},
	}
}

// --- checkbox: absent token -> evaluation ------------------------------------

// TestAbsentTokenIsEvaluation pins the default state. An instance with no token
// is doing non-production use, which §1 of the Community Source License grants
// to everyone. It is NOT an error, it must NOT block boot, and it must never be
// "fixed" into a lockout.
func TestAbsentTokenIsEvaluation(t *testing.T) {
	lic, err := Load(Config{})
	if err != nil {
		t.Fatalf("an absent license must not be an error (§1 Grant), got: %v", err)
	}
	if got := lic.Edition(); got != EditionEvaluation {
		t.Errorf("edition = %q, want %q", got, EditionEvaluation)
	}
	if got := lic.Subject(); got != SubjectUnlicensed {
		t.Errorf("subject = %q, want %q", got, SubjectUnlicensed)
	}
	if lic.ExpiresAt() != nil {
		t.Errorf("expires_at = %v, want nil (an unlicensed instance has no expiry)", lic.ExpiresAt())
	}
	if got := lic.Features(); len(got) != 0 {
		t.Errorf("features = %v, want empty", got)
	}
	if lic.Licensed() {
		t.Error("Licensed() = true with no token presented")
	}
	if !lic.ChangeDate().IsZero() {
		t.Errorf("ChangeDate() = %v, want zero — an unlicensed instance was never told which version it runs", lic.ChangeDate())
	}
	if lic.Claims() != nil {
		t.Error("Claims() must be nil when no token was presented")
	}
}

// TestNilLicenseIsEvaluation covers the rollback story: the seam is a pure
// addition behind nil-safe accessors, so a caller holding no license behaves
// exactly like a deployment that has none.
func TestNilLicenseIsEvaluation(t *testing.T) {
	var lic *License
	if got := lic.Edition(); got != EditionEvaluation {
		t.Errorf("nil.Edition() = %q, want %q", got, EditionEvaluation)
	}
	if got := lic.Subject(); got != SubjectUnlicensed {
		t.Errorf("nil.Subject() = %q, want %q", got, SubjectUnlicensed)
	}
	if !lic.Allow("anything") {
		t.Error("nil.Allow() must be true — a missing license gates nothing")
	}
	if lic.Licensed() || lic.Expired() || lic.Claims() != nil {
		t.Error("nil license must report unlicensed, unexpired, claimless")
	}
	if len(lic.Warnings()) != 0 || len(lic.Features()) != 0 {
		t.Error("nil license must return empty, non-nil slices")
	}
	if len(lic.LogAttrs()) == 0 {
		t.Error("nil.LogAttrs() must still describe the boot state")
	}
}

// --- checkbox: valid token ---------------------------------------------------

func TestValidTokenReportsItsClaims(t *testing.T) {
	s := newSigner(t)
	c := communityClaims()
	lic, err := Load(Config{
		Token:      s.mint(t, c),
		PublicKeys: s.trust(),
		Now:        mustTime(t, "2026-06-01T12:00:00Z"),
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := lic.Edition(); got != EditionCommunity {
		t.Errorf("edition = %q, want %q", got, EditionCommunity)
	}
	if got := lic.Subject(); got != "acme-lumber" {
		t.Errorf("subject = %q, want acme-lumber", got)
	}
	if got := lic.LicensedVersion(); got != "AI_LM 1.0.0" {
		t.Errorf("licensed_version = %q", got)
	}
	if got, want := lic.ChangeDate().Format("2006-01-02"), "2031-01-15"; got != want {
		t.Errorf("change_date = %s, want %s", got, want)
	}
	if got := lic.KeyID(); got != KeyID(s.pub) {
		t.Errorf("key_id = %q, want %q", got, KeyID(s.pub))
	}
	if got := lic.Features(); len(got) != 2 {
		t.Errorf("features = %v, want the two recorded", got)
	}
	if len(lic.Warnings()) != 0 {
		t.Errorf("a current, valid token must warn about nothing, got %v", lic.Warnings())
	}
	// The determination's basis is readable, which is the point of recording
	// facts rather than computing them.
	got := lic.Claims()
	if got == nil || !got.IndependentOperator || got.LocationsAtIssue != 12 ||
		got.ThresholdLocations != 50 || got.ThresholdRevenueUSD != 1_000_000_000 {
		t.Errorf("claims did not survive round-trip: %+v", got)
	}
	// Claims() hands back a copy: a handler must not be able to edit a signed
	// determination out from under the process.
	got.Subject = "somebody-else"
	got.Features[0] = "tampered"
	if lic.Subject() != "acme-lumber" || lic.Features()[0] != "ai_briefing" {
		t.Error("Claims() leaked a mutable reference to the verified claim set")
	}
}

func TestEditionsRoundTrip(t *testing.T) {
	s := newSigner(t)
	for _, ed := range []Edition{EditionEvaluation, EditionCommunity, EditionCommercial} {
		t.Run(string(ed), func(t *testing.T) {
			c := communityClaims()
			c.Edition = ed
			lic, err := Load(Config{Token: s.mint(t, c), PublicKeys: s.trust(), Now: mustTime(t, "2026-06-01T00:00:00Z")})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if lic.Edition() != ed {
				t.Errorf("edition = %q, want %q", lic.Edition(), ed)
			}
		})
	}
}

// --- checkbox: invalid token -> refuse to boot -------------------------------

// TestInvalidTokenRefusesToBoot is the adversarial table. Every one of these
// must be a HARD failure: a present-but-invalid token that quietly became
// "evaluation" would make the signature decorative, which is worse than having
// no signature at all.
func TestInvalidTokenRefusesToBoot(t *testing.T) {
	s := newSigner(t)
	other := newSigner(t)
	good := s.mint(t, communityClaims())

	// A token whose claim set was edited after issue: decode segment 3, change
	// a fact the determination rested on, re-encode, keep the signature.
	tamper := func(t *testing.T, mutate func(*Claims)) string {
		t.Helper()
		parts := strings.Split(good, ".")
		payload, err := b64.DecodeString(parts[2])
		if err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		var c Claims
		if err := json.Unmarshal(payload, &c); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		mutate(&c)
		edited, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		parts[2] = b64.EncodeToString(edited)
		return strings.Join(parts, ".")
	}

	tests := []struct {
		name    string
		token   func(t *testing.T) string
		keys    []string
		wantErr error
	}{
		{
			name:    "not a token at all",
			token:   func(*testing.T) string { return "hello" },
			keys:    s.trust(),
			wantErr: ErrMalformedToken,
		},
		{
			name:    "wrong prefix",
			token:   func(*testing.T) string { return strings.Replace(good, "ailm1.", "ailm9.", 1) },
			keys:    s.trust(),
			wantErr: ErrMalformedToken,
		},
		{
			name: "bad base64 in the claim set",
			token: func(*testing.T) string {
				p := strings.Split(good, ".")
				p[2] = "!!!not base64!!!"
				return strings.Join(p, ".")
			},
			keys:    s.trust(),
			wantErr: ErrMalformedToken,
		},
		{
			name:    "bad base64 in the signature",
			token:   func(*testing.T) string { p := strings.Split(good, "."); p[3] = "@@@"; return strings.Join(p, ".") },
			keys:    s.trust(),
			wantErr: ErrMalformedToken,
		},
		{
			name: "truncated payload",
			token: func(*testing.T) string {
				p := strings.Split(good, ".")
				p[2] = p[2][:len(p[2])/2]
				return strings.Join(p, ".")
			},
			keys:    s.trust(),
			wantErr: ErrMalformedToken,
		},
		{
			name: "truncated signature",
			token: func(*testing.T) string {
				p := strings.Split(good, ".")
				p[3] = p[3][:len(p[3])-8]
				return strings.Join(p, ".")
			},
			keys:    s.trust(),
			wantErr: ErrMalformedToken,
		},
		{
			name:    "missing a segment",
			token:   func(*testing.T) string { return strings.Join(strings.Split(good, ".")[:3], ".") },
			keys:    s.trust(),
			wantErr: ErrMalformedToken,
		},
		{
			name: "claim set is valid base64 but not a claim set",
			token: func(*testing.T) string {
				p := strings.Split(good, ".")
				p[2] = b64.EncodeToString([]byte("[1,2,3]"))
				return strings.Join(p, ".")
			},
			keys:    s.trust(),
			wantErr: ErrBadSignature, // signature is checked first; either way it must not boot
		},
		{
			name:    "signed by a key this deployment does not trust",
			token:   func(t *testing.T) string { return other.mint(t, communityClaims()) },
			keys:    s.trust(),
			wantErr: ErrUnknownSigningKey,
		},
		{
			name:    "no verification key configured at all",
			token:   func(*testing.T) string { return good },
			keys:    nil,
			wantErr: ErrNoTrustedKeys,
		},
		{
			name:    "verification key is not a key",
			token:   func(*testing.T) string { return good },
			keys:    []string{"not-a-key"},
			wantErr: nil, // any error; ParsePublicKey's own
		},
		{
			name:    "tampered: edition promoted to commercial",
			token:   func(t *testing.T) string { return tamper(t, func(c *Claims) { c.Edition = EditionCommercial }) },
			keys:    s.trust(),
			wantErr: ErrBadSignature,
		},
		{
			name:    "tampered: subject swapped to another dealer",
			token:   func(t *testing.T) string { return tamper(t, func(c *Claims) { c.Subject = "rival-lumber" }) },
			keys:    s.trust(),
			wantErr: ErrBadSignature,
		},
		{
			name: "tampered: expiry pushed out",
			token: func(t *testing.T) string {
				return tamper(t, func(c *Claims) { c.ExpiresAt = ptr(time.Now().AddDate(50, 0, 0)) })
			},
			keys:    s.trust(),
			wantErr: ErrBadSignature,
		},
		{
			name: "tampered: key id repointed at a key that IS trusted",
			token: func(*testing.T) string {
				// A forged token signed by `other`, relabelled to claim it was
				// signed by the trusted key. The kid selects; the signature decides.
				p := strings.Split(other.mint(t, communityClaims()), ".")
				p[1] = KeyID(s.pub)
				return strings.Join(p, ".")
			},
			keys:    s.trust(),
			wantErr: ErrBadSignature,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lic, err := Load(Config{Token: tc.token(t), PublicKeys: tc.keys, Now: mustTime(t, "2026-06-01T00:00:00Z")})
			if err == nil {
				t.Fatalf("an invalid token booted as %q — a present-but-invalid license must be a hard startup failure", lic.Edition())
			}
			if lic != nil {
				t.Errorf("Load returned a *License alongside an error (%v); callers must get nothing to fall back to", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want one wrapping %v", err, tc.wantErr)
			}
		})
	}
}

// TestUnreadableLicenseFileRefusesToBoot: a named-but-missing licence file is a
// misconfiguration, not an absent licence. A typo in a mount path must not
// silently downgrade the deployment.
func TestUnreadableLicenseFileRefusesToBoot(t *testing.T) {
	if _, err := Load(Config{File: filepath.Join(t.TempDir(), "nope.lic")}); err == nil {
		t.Fatal("a LICENSE_FILE that cannot be read must be a hard startup failure")
	}
	empty := filepath.Join(t.TempDir(), "empty.lic")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Config{File: empty}); err == nil {
		t.Fatal("an empty LICENSE_FILE must be a hard startup failure")
	}
}

// TestLicenseFileIsReadAndVerified covers the mounted-secret deployment shape.
func TestLicenseFileIsReadAndVerified(t *testing.T) {
	s := newSigner(t)
	path := filepath.Join(t.TempDir(), "ai_lm.lic")
	if err := os.WriteFile(path, []byte("\n"+s.mint(t, communityClaims())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lic, err := Load(Config{File: path, PublicKeys: s.trust(), Now: mustTime(t, "2026-06-01T00:00:00Z")})
	if err != nil {
		t.Fatalf("load from file: %v", err)
	}
	if lic.Edition() != EditionCommunity {
		t.Errorf("edition = %q, want %q", lic.Edition(), EditionCommunity)
	}
}

// TestTokenWinsOverFileAndSaysSo: an ambiguous configuration is resolved
// deterministically and reported, rather than resolved silently.
func TestTokenWinsOverFileAndSaysSo(t *testing.T) {
	s := newSigner(t)
	other := newSigner(t)
	path := filepath.Join(t.TempDir(), "stale.lic")
	if err := os.WriteFile(path, []byte(other.mint(t, communityClaims())), 0o600); err != nil {
		t.Fatal(err)
	}
	c := communityClaims()
	c.Subject = "from-env"
	lic, err := Load(Config{Token: s.mint(t, c), File: path, PublicKeys: s.trust(), Now: mustTime(t, "2026-06-01T00:00:00Z")})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if lic.Subject() != "from-env" {
		t.Errorf("subject = %q, want the LICENSE_TOKEN one", lic.Subject())
	}
	if len(lic.Warnings()) != 1 || !strings.Contains(lic.Warnings()[0], "LICENSE_FILE") {
		t.Errorf("warnings = %v, want one naming the ignored LICENSE_FILE", lic.Warnings())
	}
}

// TestMalformedClaimsRefuseToBoot: correctly signed, but says something a
// licence cannot say. Still a hard failure — a token this build cannot fully
// represent must not be reported as one it can.
func TestMalformedClaimsRefuseToBoot(t *testing.T) {
	s := newSigner(t)
	tests := []struct {
		name   string
		mutate func(*Claims)
	}{
		{"no issuer", func(c *Claims) { c.Issuer = "" }},
		{"no subject", func(c *Claims) { c.Subject = "" }},
		{"no licensed_version", func(c *Claims) { c.LicensedVersion = "" }},
		{"no issued_at", func(c *Claims) { c.IssuedAt = time.Time{} }},
		{"no availability_date", func(c *Claims) { c.AvailabilityDate = "" }},
		{"availability_date is not a date", func(c *Claims) { c.AvailabilityDate = "January 2026" }},
		{"unknown edition", func(c *Claims) { c.Edition = "enterprise-plus" }},
		{"agpl cannot be issued, only derived", func(c *Claims) { c.Edition = EditionAGPL }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := communityClaims()
			tc.mutate(&c)
			if _, err := Load(Config{Token: s.mint(t, c), PublicKeys: s.trust(), Now: mustTime(t, "2026-06-01T00:00:00Z")}); err == nil {
				t.Fatal("a token with an unrepresentable claim set must be a hard startup failure")
			} else if !errors.Is(err, ErrInvalidClaims) {
				t.Errorf("error = %v, want one wrapping ErrInvalidClaims", err)
			}
		})
	}
}

// TestUnknownClaimRefusesToBoot: a token minted by a newer issuer carries a
// determination this binary cannot represent. Reporting a licence it only
// partly understood would be worse than saying so at boot.
func TestUnknownClaimRefusesToBoot(t *testing.T) {
	s := newSigner(t)
	payload, err := json.Marshal(map[string]any{
		"issuer": "FutureBuild, Inc.", "subject": "acme-lumber", "edition": "community",
		"licensed_version": "AI_LM 1.0.0", "availability_date": "2026-01-15",
		"issued_at": "2026-02-01T00:00:00Z", "seat_limit_from_a_future_schema": 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.Join([]string{"ailm1", KeyID(s.pub), b64.EncodeToString(payload),
		b64.EncodeToString(ed25519.Sign(s.priv, payload))}, ".")
	if _, err := Load(Config{Token: tok, PublicKeys: s.trust()}); !errors.Is(err, ErrMalformedToken) {
		t.Fatalf("error = %v, want one wrapping ErrMalformedToken", err)
	}
}

// --- checkbox: competing_use_acknowledged -> refuse to boot -------------------

// TestCompetingUseRefusesToBoot. Competing Use is outside the grant for
// EVERYONE regardless of size and requires a separate written license the
// Steward may decline. The software must not run as though it were inside the
// grant, and the error has to name the exclusion so the operator knows this is
// a licensing decision and not a bug.
func TestCompetingUseRefusesToBoot(t *testing.T) {
	s := newSigner(t)
	for _, ed := range []Edition{EditionEvaluation, EditionCommunity, EditionCommercial} {
		t.Run(string(ed), func(t *testing.T) {
			c := communityClaims()
			c.Edition = ed
			c.CompetingUseAcknowledged = true
			lic, err := Load(Config{Token: s.mint(t, c), PublicKeys: s.trust(), Now: mustTime(t, "2026-06-01T00:00:00Z")})
			if err == nil {
				t.Fatalf("booted as %q with competing_use_acknowledged set", lic.Edition())
			}
			if !errors.Is(err, ErrCompetingUse) {
				t.Fatalf("error = %v, want one wrapping ErrCompetingUse", err)
			}
			msg := err.Error()
			for _, want := range []string{"Competing Use", "competing_use_acknowledged", "excluded", "acme-lumber"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error message must name %q; got: %s", want, msg)
				}
			}
		})
	}
}

// --- checkbox: expired token -> evaluation, loudly, still booting ------------

// TestExpiredTokenDropsToEvaluationAndStillBoots. An expiry must never take a
// dealer's dispatch offline mid-shift: v1 gates nothing, so the honest report
// IS the deliverable. The WARN has to name the date so somebody can act on it.
func TestExpiredTokenDropsToEvaluationAndStillBoots(t *testing.T) {
	s := newSigner(t)
	expiry := mustTime(t, "2026-05-31T23:59:59Z")

	tests := []struct {
		name        string
		now         string
		wantEdition Edition
		wantWarn    bool
	}{
		{"the day before expiry", "2026-05-30T12:00:00Z", EditionCommunity, false},
		{"one second before expiry", "2026-05-31T23:59:58Z", EditionCommunity, false},
		{"exactly at expiry", "2026-05-31T23:59:59Z", EditionCommunity, false},
		{"one second after expiry", "2026-06-01T00:00:00Z", EditionEvaluation, true},
		{"long after expiry", "2027-01-01T00:00:00Z", EditionEvaluation, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := communityClaims()
			c.ExpiresAt = ptr(expiry)
			lic, err := Load(Config{Token: s.mint(t, c), PublicKeys: s.trust(), Now: mustTime(t, tc.now)})
			if err != nil {
				t.Fatalf("an expired license must still boot, got error: %v", err)
			}
			if got := lic.Edition(); got != tc.wantEdition {
				t.Errorf("edition = %q, want %q", got, tc.wantEdition)
			}
			if got := lic.Expired(); got != tc.wantWarn {
				t.Errorf("Expired() = %v, want %v", got, tc.wantWarn)
			}
			// The determination itself survives: the token was real, it ran
			// out. Subject stays on the metric labels so the deployment is
			// still identifiable after expiry.
			if lic.Subject() != "acme-lumber" || !lic.Licensed() {
				t.Errorf("an expired token is still a token: subject=%q licensed=%v", lic.Subject(), lic.Licensed())
			}
			if !tc.wantWarn {
				if len(lic.Warnings()) != 0 {
					t.Errorf("warnings = %v, want none", lic.Warnings())
				}
				return
			}
			if len(lic.Warnings()) == 0 {
				t.Fatal("an expired license must warn; a silent downgrade is not a report")
			}
			w := strings.Join(lic.Warnings(), " | ")
			if !strings.Contains(w, expiry.UTC().Format(time.RFC3339)) {
				t.Errorf("the warning must name the expiry date %s; got: %s", expiry.UTC().Format(time.RFC3339), w)
			}
		})
	}
}

// --- checkbox: Change Date ---------------------------------------------------

// TestChangeDateConversion. On the Change Date a version's terms are replaced
// by AGPL-3.0-only, automatically and for everyone (§4). That is a fact about
// the version, so it outranks every other claim in the token — including a
// commercial edition and including an expiry.
func TestChangeDateConversion(t *testing.T) {
	s := newSigner(t)

	tests := []struct {
		name         string
		availability string
		edition      Edition
		expires      *time.Time
		now          string
		want         Edition
	}{
		// The boundary is exactly the one the spec fixes: agpl when
		// now > availability_date + 5 years, where the availability date is
		// UTC midnight. So the instant the Change Date begins is still the old
		// terms, and any moment after it is not.
		{"five years less a day", "2026-01-15", EditionCommunity, nil, "2031-01-14T12:00:00Z", EditionCommunity},
		{"the first instant of the change date", "2026-01-15", EditionCommunity, nil, "2031-01-15T00:00:00Z", EditionCommunity},
		{"one second into the change date", "2026-01-15", EditionCommunity, nil, "2031-01-15T00:00:01Z", EditionAGPL},
		{"the last instant of the change date", "2026-01-15", EditionCommunity, nil, "2031-01-15T23:59:59Z", EditionAGPL},
		{"one day past the change date", "2026-01-15", EditionCommunity, nil, "2031-01-16T00:00:01Z", EditionAGPL},
		{"long past, commercial", "2015-06-30", EditionCommercial, nil, "2026-06-01T00:00:00Z", EditionAGPL},
		{"long past, evaluation", "2015-06-30", EditionEvaluation, nil, "2026-06-01T00:00:00Z", EditionAGPL},
		{"converted AND expired: conversion wins", "2015-06-30", EditionCommercial,
			ptr(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)), "2026-06-01T00:00:00Z", EditionAGPL},
		{"leap-day availability", "2028-02-29", EditionCommunity, nil, "2033-03-01T00:00:01Z", EditionAGPL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := communityClaims()
			c.AvailabilityDate = tc.availability
			c.Edition = tc.edition
			c.ExpiresAt = tc.expires
			lic, err := Load(Config{Token: s.mint(t, c), PublicKeys: s.trust(), Now: mustTime(t, tc.now)})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := lic.Edition(); got != tc.want {
				t.Errorf("edition = %q, want %q (change date %s, now %s)",
					got, tc.want, lic.ChangeDate().Format("2006-01-02"), tc.now)
			}
		})
	}
}

// TestChangeDateIsAvailabilityPlusFive pins the arithmetic itself, including
// the leap-year case where "+5 years" is not "+1826 days".
func TestChangeDateIsAvailabilityPlusFive(t *testing.T) {
	s := newSigner(t)
	tests := []struct{ availability, want string }{
		{"2026-01-15", "2031-01-15"},
		{"2026-12-31", "2031-12-31"},
		{"2028-02-29", "2033-03-01"}, // 2033 has no 29 February; Go normalises forward
	}
	for _, tc := range tests {
		t.Run(tc.availability, func(t *testing.T) {
			c := communityClaims()
			c.AvailabilityDate = tc.availability
			lic, err := Load(Config{Token: s.mint(t, c), PublicKeys: s.trust(), Now: mustTime(t, "2026-06-01T00:00:00Z")})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := lic.ChangeDate().Format("2006-01-02"); got != tc.want {
				t.Errorf("ChangeDate() = %s, want %s", got, tc.want)
			}
			if lic.ChangeDate().Location() != time.UTC {
				t.Errorf("ChangeDate() must be UTC, got %v", lic.ChangeDate().Location())
			}
		})
	}
}

// --- checkbox: the feature gate gates nothing --------------------------------

// TestAllowGatesNothing is a tripwire, not a behaviour test.
//
// v1 reserves the entitlement seam and gates NOTHING: Allow returns true for
// every feature in every edition, including for a deployment with no license at
// all. That is deliberate — the community edition is unrestricted by design,
// and the value here is that the seam exists to be changed later.
//
// If you are reading this because you just made this test fail: good, that is
// what it is for. Gating a feature is a product decision. Make it in a diff
// that says so, and rewrite this test to describe the new policy explicitly.
func TestAllowGatesNothing(t *testing.T) {
	s := newSigner(t)

	features := []string{
		"", "ai_briefing", "ors_routing", "multi_tenant", "commercial_only",
		"a-feature-nobody-has-invented-yet", "*",
	}

	licenses := map[string]*License{}
	unlicensed, err := Load(Config{})
	if err != nil {
		t.Fatal(err)
	}
	licenses["unlicensed"] = unlicensed
	licenses["nil"] = nil

	for _, ed := range []Edition{EditionEvaluation, EditionCommunity, EditionCommercial} {
		c := communityClaims()
		c.Edition = ed
		c.Features = nil // no features recorded at all
		lic, err := Load(Config{Token: s.mint(t, c), PublicKeys: s.trust(), Now: mustTime(t, "2026-06-01T00:00:00Z")})
		if err != nil {
			t.Fatal(err)
		}
		licenses[string(ed)] = lic
	}

	expired := communityClaims()
	expired.ExpiresAt = ptr(mustTime(t, "2020-01-01T00:00:00Z"))
	exp, err := Load(Config{Token: s.mint(t, expired), PublicKeys: s.trust(), Now: mustTime(t, "2026-06-01T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	licenses["expired"] = exp

	converted := communityClaims()
	converted.AvailabilityDate = "2015-01-01"
	conv, err := Load(Config{Token: s.mint(t, converted), PublicKeys: s.trust(), Now: mustTime(t, "2026-06-01T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	licenses["agpl"] = conv

	for name, lic := range licenses {
		for _, f := range features {
			if !lic.Allow(f) {
				t.Errorf("Allow(%q) = false for the %s license; v1 gates NOTHING — "+
					"if this is a deliberate policy change, rewrite this test to say so", f, name)
			}
		}
	}
}

// --- boot line ---------------------------------------------------------------

// TestLogAttrsDescribeTheBootState pins the single INFO line cmd/server emits.
// The operator walkthrough reads these five values off the log, so an absent
// license has to produce readable words rather than empty strings.
func TestLogAttrsDescribeTheBootState(t *testing.T) {
	s := newSigner(t)

	unlicensed, err := Load(Config{})
	if err != nil {
		t.Fatal(err)
	}
	got := attrMap(t, unlicensed.LogAttrs())
	want := map[string]string{
		"edition": "evaluation", "subject": "unlicensed", "licensed_version": "none",
		"expires_at": "never", "change_date": "unknown",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("unlicensed boot line %s = %q, want %q", k, got[k], v)
		}
	}

	c := communityClaims()
	c.ExpiresAt = ptr(mustTime(t, "2027-02-01T00:00:00Z"))
	lic, err := Load(Config{Token: s.mint(t, c), PublicKeys: s.trust(), Now: mustTime(t, "2026-06-01T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	got = attrMap(t, lic.LogAttrs())
	want = map[string]string{
		"edition": "community", "subject": "acme-lumber", "licensed_version": "AI_LM 1.0.0",
		"expires_at": "2027-02-01T00:00:00Z", "change_date": "2031-01-15",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("licensed boot line %s = %q, want %q", k, got[k], v)
		}
	}
}

func attrMap(t *testing.T, attrs []any) map[string]string {
	t.Helper()
	if len(attrs)%2 != 0 {
		t.Fatalf("LogAttrs returned an odd number of values: %v", attrs)
	}
	out := make(map[string]string, len(attrs)/2)
	for i := 0; i < len(attrs); i += 2 {
		k, ok := attrs[i].(string)
		if !ok {
			t.Fatalf("LogAttrs key %v is not a string", attrs[i])
		}
		out[k] = attrs[i+1].(string)
	}
	return out
}

// --- key handling ------------------------------------------------------------

func TestKeyRotationAcceptsEitherKey(t *testing.T) {
	oldKey, newKey := newSigner(t), newSigner(t)
	both := []string{EncodeKey(oldKey.pub), EncodeKey(newKey.pub)}
	for name, s := range map[string]signer{"old": oldKey, "new": newKey} {
		t.Run(name, func(t *testing.T) {
			lic, err := Load(Config{Token: s.mint(t, communityClaims()), PublicKeys: both, Now: mustTime(t, "2026-06-01T00:00:00Z")})
			if err != nil {
				t.Fatalf("during a rotation both keys must verify: %v", err)
			}
			if lic.KeyID() != KeyID(s.pub) {
				t.Errorf("key_id = %q, want %q", lic.KeyID(), KeyID(s.pub))
			}
		})
	}
}

func TestSplitKeys(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"   ", 0},
		{"AAAA", 1},
		{"AAAA,BBBB", 2},
		{"AAAA, BBBB", 2},
		{" AAAA \n BBBB ", 2},
		{"AAAA;BBBB;CCCC", 3},
	}
	for _, tc := range tests {
		if got := SplitKeys(tc.in); len(got) != tc.want {
			t.Errorf("SplitKeys(%q) = %v, want %d keys", tc.in, got, tc.want)
		}
	}
}

func TestKeyEncodingRoundTrip(t *testing.T) {
	s := newSigner(t)
	std := EncodeKey(s.pub)
	for _, form := range []string{
		std,
		strings.TrimRight(std, "="),
		strings.NewReplacer("+", "-", "/", "_").Replace(std),
		"  " + std + "\n",
	} {
		pub, err := ParsePublicKey(form)
		if err != nil {
			t.Fatalf("ParsePublicKey(%q): %v", form, err)
		}
		if KeyID(pub) != KeyID(s.pub) {
			t.Errorf("round-trip changed the key id for %q", form)
		}
	}
	if _, err := ParsePublicKey(EncodeKey(s.priv)); err == nil {
		t.Error("a private key must not parse as a public key")
	}
	priv, err := ParsePrivateKey(EncodeKey(s.priv))
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	if !priv.Equal(s.priv) {
		t.Error("private key did not round-trip")
	}
	if _, err := ParsePrivateKey(EncodeKey(s.priv.Seed())); err != nil {
		t.Errorf("a 32-byte seed must expand to a private key: %v", err)
	}
	if _, err := Mint(communityClaims(), ed25519.PrivateKey("too short")); err == nil {
		t.Error("Mint must reject a key of the wrong size")
	}
}

// TestKeyIDIsDerivedNotAssigned: the key id is a function of the key, so a
// deployment configures a key and never a key *name*. Nothing to keep in sync.
func TestKeyIDIsDerivedNotAssigned(t *testing.T) {
	s := newSigner(t)
	if KeyID(s.pub) != KeyID(s.pub) {
		t.Fatal("KeyID is not deterministic")
	}
	if len(KeyID(s.pub)) != 16 {
		t.Errorf("KeyID = %q, want 16 hex characters", KeyID(s.pub))
	}
	if KeyID(s.pub) == KeyID(newSigner(t).pub) {
		t.Error("two different keys share a key id")
	}
}
