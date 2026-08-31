// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package license is AI_LM's entitlement seam: it reads an offline, signed
// licence token, reports which edition this deployment is running under, and
// answers feature-gate questions.
//
// # It gates nothing in v1
//
// Allow reports true for every feature in every edition. The package exists so
// that the day a capability becomes commercial-only, it is one call site and a
// reviewed diff — not a retrofit through a system that has already shipped
// without any notion of entitlement. license_test.go pins the always-true
// behaviour explicitly so that change cannot happen by accident.
//
// # The editions come from the licence, not from a price list
//
// AI_LM is licensed under LicenseRef-OpenLBM-Community-Source-1.0 (see
// LICENSES/ in the repository root). That licence already defines the states
// this software can be in, so the seam encodes them rather than running a
// parallel vocabulary:
//
//	evaluation  §1 Grant                     anyone, non-production. NO TOKEN REQUIRED.
//	community   §2 Additional Use Grant      a Community Member, in production, at no charge.
//	commercial  §3 Production-Use Condition  everyone else in production, under a separate licence.
//	agpl        §4 Change Date / Conversion  any version past its Change Date.
//
// An unlicensed instance therefore reports "evaluation" and boots normally.
// That is not a fail-open loophole to be tightened later: §1 grants
// non-production use to everyone, so an instance with no token is exercising a
// grant it already holds. A PRESENT but invalid token is a different question
// and fails closed, hard.
//
// # The token records a determination; it does not compute one
//
// "Is this organisation a Community Member?" and "is this Competing Use?" are
// legal judgements about corporate control and intent. Software cannot decide
// them and code that pretends to will be confidently wrong. The token
// therefore carries the FACTS the determination rested on — the location count
// and revenue answer as given, and the thresholds that were in force on the day
// — signed by the issuer, so an auditor can reconstruct why an edition was
// granted even after the thresholds move.
//
// # No network, ever
//
// Nothing in this package makes a network call, on any path. Self-hosted AI_LM
// runs on dealer networks that are not always reachable, and an unsolicited
// phone-home from software somebody else is hosting is a trust problem, not a
// feature. imports_test.go fails the build if this package ever imports
// net/http.
package license

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"strings"
	"time"
)

// Edition is the licence state this deployment is running under. See the
// package doc for each one's basis in the licence text.
type Edition string

const (
	// EditionEvaluation is §1: non-production use, granted to everyone, no
	// token required. It is the default and it is a legitimate state.
	EditionEvaluation Edition = "evaluation"

	// EditionCommunity is §2, the community carve-out: a Community Member
	// using AI_LM in production at no charge.
	EditionCommunity Edition = "community"

	// EditionCommercial is §3: production use by an organisation that is not a
	// Community Member, under a separate commercial licence.
	EditionCommercial Edition = "commercial"

	// EditionAGPL is §4: this version is past its Change Date and is now
	// governed by AGPL-3.0-only, automatically and for everyone. It is derived
	// from the availability date, never issued in a token.
	EditionAGPL Edition = "agpl"
)

// SubjectUnlicensed is the subject label used when no token is present. It is
// a deliberate, readable value rather than an empty string, because it appears
// on Prometheus labels where "" is indistinguishable from a bug.
const SubjectUnlicensed = "unlicensed"

// ChangeDateYears is the Change Date interval from the Parameters block of
// LicenseRef-OpenLBM-Community-Source-1.0: five years from the date the
// specific version was first made available.
const ChangeDateYears = 5

// availabilityDateLayout is the wire format of Claims.AvailabilityDate. A plain
// calendar date, because "the date this version was first made available" is a
// date and not an instant.
const availabilityDateLayout = "2006-01-02"

// Claims is the signed claim set. Every field is a fact recorded at issue, not
// a computation performed at boot.
type Claims struct {
	// Issuer is who made the determination (FutureBuild, Inc.), and Subject is
	// the deployment it was made about — a dealer, not a user. With one stack
	// per dealer, the deployment IS the tenant, which is why metering needs no
	// tenancy model and no migration.
	Issuer  string  `json:"issuer"`
	Subject string  `json:"subject"`
	Edition Edition `json:"edition"`

	// LicensedVersion names the Licensed Work this token covers ("AI_LM 2.3.0"),
	// and AvailabilityDate (YYYY-MM-DD) is the day that version was first made
	// available. AvailabilityDate is MANDATORY: conversion to the Change
	// License is per-version, so a token that cannot say when its version
	// became available cannot say when it converts — and that date is cheap to
	// record now and impossible to reconstruct later.
	LicensedVersion  string `json:"licensed_version"`
	AvailabilityDate string `json:"availability_date"`

	// IndependentOperator is the size determination AS MADE by the issuer, and
	// the two fields under it are the facts it rested on. LocationsAtIssue is
	// the count on the day of issue; a dealer that opens its fiftieth branch
	// next year has not retroactively falsified this token, it has changed
	// circumstances that the licence's own cure period covers (§7).
	IndependentOperator     bool `json:"independent_operator"`
	LocationsAtIssue        int  `json:"locations_at_issue"`
	ControlledRevenueOver1B bool `json:"controlled_revenue_over_1b"`

	// ThresholdLocations and ThresholdRevenueUSD are the thresholds IN FORCE AT
	// ISSUE, carried in the token rather than compiled into this binary. The
	// Standard still marks 50 / $1B [COUNSEL: thresholds are business
	// parameters]; if they move, every previously-issued token's basis has to
	// stay interpretable, and it only does if the token says what it was
	// measured against.
	ThresholdLocations  int   `json:"threshold_locations"`
	ThresholdRevenueUSD int64 `json:"threshold_revenue_usd"`

	// ProductionUse records whether this deployment was licensed for a
	// Production Purpose (§3's trigger) or for evaluation only.
	ProductionUse bool `json:"production_use"`

	// CompetingUseAcknowledged must be false. A true value is an issuer
	// recording that this deployment's use falls under the field-of-use
	// exclusion — outside the grant for everyone regardless of size — and the
	// software must refuse to run rather than imply it can represent that
	// state. See Load.
	CompetingUseAcknowledged bool `json:"competing_use_acknowledged"`

	// IssuedAt and ExpiresAt bound the determination in time. ExpiresAt is
	// optional: a perpetual commercial licence has no expiry.
	IssuedAt  time.Time  `json:"issued_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// Features is the entitlement list. It is recorded and reported; nothing
	// consults it in v1, because Allow gates nothing.
	Features []string `json:"features,omitempty"`
}

// Config is what Load needs. It is deliberately plain data with no environment
// reading of its own beyond LICENSE_FILE, so the whole package is testable
// without touching the process environment.
type Config struct {
	// Token is the token itself (LICENSE_TOKEN). It wins over File when both
	// are supplied, and Load records a warning saying so.
	Token string

	// File is a path to read the token from (LICENSE_FILE), for deployments
	// that mount a secret rather than set an environment variable.
	File string

	// PublicKeys are the base64 Ed25519 verification keys this deployment
	// trusts (LICENSE_PUBLIC_KEY, comma- or whitespace-separated; see
	// SplitKeys). More than one is normal during a key rotation. The signing
	// PRIVATE key never appears here, in the repository, or on this host.
	PublicKeys []string

	// Now overrides the clock. Zero means time.Now().UTC(). It exists so
	// expiry and the Change Date can be table-tested across their boundaries
	// rather than being tested by waiting five years.
	Now time.Time
}

// builtinPublicKeys is where FutureBuild's production verification key is
// embedded once one exists. It is empty today, which means a deployment that
// presents a token MUST configure LICENSE_PUBLIC_KEY — and one that presents no
// token is unaffected, because nothing needs verifying. Keeping it empty rather
// than shipping a placeholder is the point: a placeholder key would be a
// trusted issuer nobody controls.
var builtinPublicKeys []string

// License is the resolved entitlement. The zero value is not valid — use Load,
// which is the only constructor. Every accessor is nil-safe and reports the
// unlicensed defaults, so a caller that was handed no licence behaves exactly
// like a deployment that has none rather than panicking.
type License struct {
	edition  Edition
	claims   *Claims // nil when no token was presented
	keyID    string
	now      time.Time
	warnings []string
}

// Load reads and verifies the licence for this deployment.
//
// It returns an error ONLY when the deployment must not start:
//
//   - a token was presented and could not be verified — malformed, bad
//     signature, an unknown signing key, a claim edited after issue, or no
//     trusted key configured at all. A present-but-invalid token is never
//     downgraded to a lesser edition: silently continuing would make the
//     signature decorative, which is worse than having none.
//   - a verified token has competing_use_acknowledged: true. Competing Use is
//     outside the grant for everyone regardless of size and requires a separate
//     written licence the Steward may decline; software that ran anyway would be
//     representing a state that does not exist.
//
// Everything else boots:
//
//   - no token at all is a valid, unremarkable state: edition "evaluation",
//     subject "unlicensed", no expiry, no features (§1 Grant).
//   - an EXPIRED token drops to "evaluation" and records a warning naming the
//     expiry date. It must not take a dealer's dispatch offline mid-shift, and
//     since v1 gates nothing, nothing breaks — the honest report is the whole
//     deliverable.
//   - a version past its Change Date reports "agpl" regardless of every other
//     claim, because conversion is automatic and for everyone (§4). That is a
//     fact about the version, not a grant this software hands out.
func Load(cfg Config) (*License, error) {
	now := cfg.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	l := &License{edition: EditionEvaluation, now: now}

	token, warn, err := readToken(cfg)
	if err != nil {
		return nil, err
	}
	l.warnings = append(l.warnings, warn...)

	// No token: §1 grants non-production use to everyone. Boot.
	if token == "" {
		return l, nil
	}

	trusted, err := trustedKeys(cfg.PublicKeys)
	if err != nil {
		return nil, err
	}
	if len(trusted) == 0 {
		return nil, ErrNoTrustedKeys
	}

	claims, kid, err := verify(token, trusted)
	if err != nil {
		return nil, err
	}
	if err := validate(claims); err != nil {
		return nil, err
	}

	// Field-of-use exclusion. Checked before anything else is reported so no
	// edition is ever computed for a use that is outside the grant entirely.
	if claims.CompetingUseAcknowledged {
		return nil, fmt.Errorf(
			"%w: token for %q sets competing_use_acknowledged — Competing Use (using the Licensed Work to develop, "+
				"train, enhance or offer a product that competes with the Gable/OpenLBM offerings in the vertical) is "+
				"excluded from the grant for everyone regardless of size, and requires a separate written license from "+
				"the Steward which may be declined. Refusing to start",
			ErrCompetingUse, claims.Subject)
	}

	l.claims = claims
	l.keyID = kid
	l.edition = claims.Edition

	// Expiry: report honestly, keep running. The dispatch board stays up.
	if exp := claims.ExpiresAt; exp != nil && now.After(*exp) {
		l.edition = EditionEvaluation
		l.warnings = append(l.warnings, fmt.Sprintf(
			"license for %q expired on %s and no longer grants the %q edition; reporting %q (non-production use under §1). "+
				"Nothing is gated in this version, so dispatch is unaffected — renew before it is",
			claims.Subject, exp.UTC().Format(time.RFC3339), claims.Edition, EditionEvaluation))
	}

	// Change Date: §4 conversion is automatic and for everyone. It outranks
	// every other claim, including an expiry — an expired token for a converted
	// version is still a converted version.
	if cd := l.ChangeDate(); !cd.IsZero() && now.After(cd) {
		l.edition = EditionAGPL
		l.warnings = append(l.warnings, fmt.Sprintf(
			"licensed version %q passed its Change Date on %s: it is now governed by the Change License (AGPL-3.0-only), "+
				"automatically and for everyone (§4)",
			claims.LicensedVersion, cd.Format(availabilityDateLayout)))
	}

	return l, nil
}

// readToken resolves the token text from LICENSE_TOKEN or LICENSE_FILE.
func readToken(cfg Config) (token string, warnings []string, err error) {
	inline := strings.TrimSpace(cfg.Token)
	path := strings.TrimSpace(cfg.File)

	switch {
	case inline != "" && path != "":
		warnings = append(warnings, "both LICENSE_TOKEN and LICENSE_FILE are set; using LICENSE_TOKEN and ignoring "+path)
		return inline, warnings, nil
	case inline != "":
		return inline, nil, nil
	case path == "":
		return "", nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		// A named-but-unreadable licence file is a misconfiguration, not an
		// absent licence. Falling back to "evaluation" here would turn a typo
		// in a mount path into a silent downgrade.
		return "", nil, fmt.Errorf("license: read LICENSE_FILE %s: %w", path, err)
	}
	token = strings.TrimSpace(string(raw))
	if token == "" {
		return "", nil, fmt.Errorf("license: LICENSE_FILE %s is empty", path)
	}
	return token, nil, nil
}

// trustedKeys indexes the configured verification keys by their derived key id.
func trustedKeys(configured []string) (map[string]ed25519.PublicKey, error) {
	all := make([]string, 0, len(builtinPublicKeys)+len(configured))
	all = append(all, builtinPublicKeys...)
	all = append(all, configured...)

	out := make(map[string]ed25519.PublicKey, len(all))
	for _, enc := range all {
		if strings.TrimSpace(enc) == "" {
			continue
		}
		pub, err := ParsePublicKey(enc)
		if err != nil {
			return nil, err
		}
		out[KeyID(pub)] = pub
	}
	return out, nil
}

// validate rejects a verified token that says something a licence cannot say.
// It runs only on claims whose signature has already been checked, so every
// failure here is a badly-minted token rather than an attack.
func validate(c *Claims) error {
	if strings.TrimSpace(c.Issuer) == "" {
		return fmt.Errorf("%w: issuer is required", ErrInvalidClaims)
	}
	if strings.TrimSpace(c.Subject) == "" {
		return fmt.Errorf("%w: subject is required", ErrInvalidClaims)
	}
	if strings.TrimSpace(c.LicensedVersion) == "" {
		return fmt.Errorf("%w: licensed_version is required", ErrInvalidClaims)
	}
	if c.IssuedAt.IsZero() {
		return fmt.Errorf("%w: issued_at is required", ErrInvalidClaims)
	}
	switch c.Edition {
	case EditionEvaluation, EditionCommunity, EditionCommercial:
	case EditionAGPL:
		// Conversion is derived from the availability date under §4. An issuer
		// cannot grant it, so a token claiming it is malformed rather than
		// generous.
		return fmt.Errorf("%w: edition %q is derived from the Change Date, not issued in a token", ErrInvalidClaims, c.Edition)
	default:
		return fmt.Errorf("%w: unknown edition %q (want %q, %q or %q)",
			ErrInvalidClaims, c.Edition, EditionEvaluation, EditionCommunity, EditionCommercial)
	}
	if strings.TrimSpace(c.AvailabilityDate) == "" {
		return fmt.Errorf("%w: availability_date is required — without it this build cannot say when it converts to the Change License", ErrInvalidClaims)
	}
	if _, err := time.Parse(availabilityDateLayout, c.AvailabilityDate); err != nil {
		return fmt.Errorf("%w: availability_date %q is not %s", ErrInvalidClaims, c.AvailabilityDate, availabilityDateLayout)
	}
	return nil
}

// Edition reports the licence state. A nil *License reports evaluation, which
// is what a deployment with no licence at all is running under.
func (l *License) Edition() Edition {
	if l == nil || l.edition == "" {
		return EditionEvaluation
	}
	return l.edition
}

// Subject is the deployment this licence was issued to, or SubjectUnlicensed.
func (l *License) Subject() string {
	if l == nil || l.claims == nil || l.claims.Subject == "" {
		return SubjectUnlicensed
	}
	return l.claims.Subject
}

// Issuer is who made the determination, or "" when there is no token.
func (l *License) Issuer() string {
	if l == nil || l.claims == nil {
		return ""
	}
	return l.claims.Issuer
}

// LicensedVersion names the Licensed Work this token covers, or "".
func (l *License) LicensedVersion() string {
	if l == nil || l.claims == nil {
		return ""
	}
	return l.claims.LicensedVersion
}

// KeyID is the signing key that was actually verified, or "". Recorded so an
// operator can tell which side of a key rotation a running instance is on.
func (l *License) KeyID() string {
	if l == nil {
		return ""
	}
	return l.keyID
}

// Licensed reports whether a token was presented AND verified. It is false for
// an unlicensed instance and true for an expired one — an expired token is a
// real determination that has run out, not an absent one.
func (l *License) Licensed() bool { return l != nil && l.claims != nil }

// AvailabilityDate is the day this version was first made available (UTC
// midnight), or the zero time when there is no token.
func (l *License) AvailabilityDate() time.Time {
	if l == nil || l.claims == nil {
		return time.Time{}
	}
	d, err := time.ParseInLocation(availabilityDateLayout, l.claims.AvailabilityDate, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return d
}

// ChangeDate is the day this version converts to the Change License
// (AGPL-3.0-only): its availability date plus ChangeDateYears.
//
// It is the zero time when there is no token, because the conversion is
// per-version and an unlicensed instance has not been told which version it is
// running. A caller must treat the zero value as "unknown", never as "already
// converted" — Load does, and reports agpl only for a known, passed date.
func (l *License) ChangeDate() time.Time {
	avail := l.AvailabilityDate()
	if avail.IsZero() {
		return time.Time{}
	}
	return avail.AddDate(ChangeDateYears, 0, 0)
}

// ExpiresAt is when the determination runs out, or nil for no expiry.
func (l *License) ExpiresAt() *time.Time {
	if l == nil || l.claims == nil || l.claims.ExpiresAt == nil {
		return nil
	}
	exp := l.claims.ExpiresAt.UTC()
	return &exp
}

// Expired reports whether a present token's expiry has passed. False when
// there is no token or no expiry.
func (l *License) Expired() bool {
	exp := l.ExpiresAt()
	return exp != nil && l.now.After(*exp)
}

// Features lists the entitlements recorded in the token. Never nil.
func (l *License) Features() []string {
	if l == nil || l.claims == nil || len(l.claims.Features) == 0 {
		return []string{}
	}
	out := make([]string, len(l.claims.Features))
	copy(out, l.claims.Features)
	return out
}

// Allow is the feature gate.
//
// IT RETURNS TRUE FOR EVERY FEATURE IN EVERY EDITION. That is the whole point
// of v1: the seam is reserved and wired, and nothing is gated. Retrofitting
// entitlement into a system that has already shipped without it is materially
// harder than adding an always-true check today.
//
// When that changes, it changes HERE, in one place, as a deliberate diff — and
// TestAllowGatesNothing will fail until the change is reviewed and the test is
// updated to describe the new policy.
func (l *License) Allow(feature string) bool { return true }

// Claims returns a copy of the verified claim set, or nil when no token was
// presented. It is a copy so a handler cannot edit a signed determination.
func (l *License) Claims() *Claims {
	if l == nil || l.claims == nil {
		return nil
	}
	c := *l.claims
	if c.ExpiresAt != nil {
		exp := *c.ExpiresAt
		c.ExpiresAt = &exp
	}
	if len(c.Features) > 0 {
		f := make([]string, len(c.Features))
		copy(f, c.Features)
		c.Features = f
	}
	return &c
}

// Warnings are the things an operator needs to be told at boot but which do not
// stop the service — an expired token, a converted version, a config
// ambiguity. cmd/server logs each at WARN. Anything that should stop the
// service is an error from Load instead.
func (l *License) Warnings() []string {
	if l == nil || len(l.warnings) == 0 {
		return []string{}
	}
	out := make([]string, len(l.warnings))
	copy(out, l.warnings)
	return out
}

// LogAttrs is the key/value tail of the single INFO line cmd/server logs at
// boot. It lives here so the boot line and the metric labels are derived from
// the same accessors and cannot drift apart.
func (l *License) LogAttrs() []any {
	attrs := []any{
		"edition", string(l.Edition()),
		"subject", l.Subject(),
		"licensed_version", orNone(l.LicensedVersion()),
	}
	if exp := l.ExpiresAt(); exp != nil {
		attrs = append(attrs, "expires_at", exp.Format(time.RFC3339))
	} else {
		attrs = append(attrs, "expires_at", "never")
	}
	if cd := l.ChangeDate(); !cd.IsZero() {
		attrs = append(attrs, "change_date", cd.Format(availabilityDateLayout))
	} else {
		attrs = append(attrs, "change_date", "unknown")
	}
	return attrs
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
