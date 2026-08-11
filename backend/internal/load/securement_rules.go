// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"math"
	"strings"
)

// SecurementRuleset is one jurisdiction's cargo-securement rule set (T1-5/T2-7).
// It is pure data so a new jurisdiction is added to the registry below without
// touching the securement solver.
type SecurementRuleset struct {
	Code string `json:"code"` // US_FMCSA, CA_NSC, ...
	Name string `json:"name"`
	// Basis is the citation surfaced to yard staff so the recommendation is
	// auditable.
	Basis string `json:"basis"`
	// AggregateWLLFraction: aggregate working load limit must be ≥ this fraction
	// of the cargo weight.
	AggregateWLLFraction float64 `json:"aggregate_wll_fraction"`
	// Tie-down count by article length: BaseTieDowns cover the first
	// FirstSegmentFt, then one more per AdditionalPerFt (or fraction) beyond it.
	BaseTieDowns    int     `json:"base_tie_downs"`
	FirstSegmentFt  float64 `json:"first_segment_ft"`
	AdditionalPerFt float64 `json:"additional_per_ft"`
	// MaxSpacingFt: when > 0, a tie-down at least every N ft along the load,
	// independent of the length-segment count (a stricter provincial rule).
	MaxSpacingFt float64 `json:"max_spacing_ft,omitempty"`
	// MaxWeightPerTieDownLbs: when > 0, at least one tie-down per N lb of cargo.
	MaxWeightPerTieDownLbs int64 `json:"max_weight_per_tie_down_lbs,omitempty"`
}

// securementRulesets is the configurable jurisdiction registry. US FMCSA and
// Canadian NSC Standard 10 are intentionally harmonized on the headline numbers
// (≥50% aggregate WLL; a tie-down per 10 ft); the Canadian set additionally
// encodes an explicit max-spacing rule to demonstrate jurisdiction-specific
// extension. Add an entry here to support another jurisdiction.
var securementRulesets = map[string]SecurementRuleset{
	"US_FMCSA": {
		Code:                 "US_FMCSA",
		Name:                 "US FMCSA — 49 CFR §393",
		Basis:                "FMCSA 49 CFR §393.106(d): aggregate WLL ≥ 50% of cargo weight; §393.110: 2 tie-downs for the first 10 ft of article length + 1 per additional 10 ft (or fraction).",
		AggregateWLLFraction: 0.50,
		BaseTieDowns:         2,
		FirstSegmentFt:       10,
		AdditionalPerFt:      10,
	},
	"CA_NSC": {
		Code:                 "CA_NSC",
		Name:                 "Canada NSC Standard 10",
		Basis:                "NSC Standard 10 / provincial MTO: aggregate WLL ≥ 50% of cargo weight; 2 tie-downs for the first 10 ft + 1 per additional 10 ft, with a tie-down at least every 10 ft of load.",
		AggregateWLLFraction: 0.50,
		BaseTieDowns:         2,
		FirstSegmentFt:       10,
		AdditionalPerFt:      10,
		MaxSpacingFt:         10,
	},
}

// defaultSecurementJurisdiction is used when none is configured or the
// configured code is unknown.
const defaultSecurementJurisdiction = "US_FMCSA"

// resolveSecurementRuleset returns the ruleset for a jurisdiction code,
// defaulting to US FMCSA when blank or unrecognized.
func resolveSecurementRuleset(code string) SecurementRuleset {
	if rs, ok := securementRulesets[strings.ToUpper(strings.TrimSpace(code))]; ok {
		return rs
	}
	return securementRulesets[defaultSecurementJurisdiction]
}

// requiredTieDowns is the minimum tie-down count the ruleset mandates for an
// article of spanFt feet weighing cargoLbs, taking the strictest of the length,
// max-spacing and per-weight rules.
func (rs SecurementRuleset) requiredTieDowns(spanFt, cargoLbs float64) int {
	n := rs.BaseTieDowns
	if spanFt > rs.FirstSegmentFt && rs.AdditionalPerFt > 0 {
		n = rs.BaseTieDowns + int(math.Ceil((spanFt-rs.FirstSegmentFt)/rs.AdditionalPerFt))
	}
	if rs.MaxSpacingFt > 0 {
		// Tie-downs at both ends plus enough to keep every gap ≤ MaxSpacingFt.
		bySpacing := int(math.Ceil(spanFt/rs.MaxSpacingFt)) + 1
		if bySpacing > n {
			n = bySpacing
		}
	}
	if rs.MaxWeightPerTieDownLbs > 0 {
		byWeight := int(math.Ceil(cargoLbs / float64(rs.MaxWeightPerTieDownLbs)))
		if byWeight > n {
			n = byWeight
		}
	}
	if n < rs.BaseTieDowns {
		n = rs.BaseTieDowns
	}
	return n
}
