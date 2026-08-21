// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Reason codes for Unplaced.Reason.
//
// Two families, and the difference is the whole point of the type:
//
//   - ReasonNoGeometry is an INFORMATION gap. The article has no recorded
//     length/width/height (a joist hanger, a tube of caulk, random-length
//     linear-foot stock), so there is no box to position. It is not a statement
//     about the truck: the material still rides, the yard just loads it by hand
//     and it never appears in the 3D twin. Migration 080 in the host ERP made
//     those dimension columns nullable precisely so "not measured" stays
//     distinguishable from a real zero, and catalog.resolveGeometry carries that
//     through as GeometryFallback/HasGeometry=false.
//   - every other reason is a CAPACITY failure. The packer had geometry, tried
//     to position it, and the truck ran out of deck, headroom, volume budget or
//     stackable surface. That cargo stays in the yard and the customer ships
//     short, so it must block a push.
const (
	ReasonNoGeometry   = "NO_GEOMETRY"
	ReasonTruckFull    = "TRUCK_FULL"
	ReasonVolumeFull   = "BED_VOLUME_FULL"
	ReasonTooLarge     = "TOO_LARGE_FOR_BED"
	ReasonNotStackable = "NOT_STACKABLE"
)

// Unplaced is one article the packer did not position, with a typed reason.
//
// Callers MUST branch on Reason (via Blocking/Rides) rather than on Detail:
// Detail is operator prose and may be reworded, Reason is the contract.
type Unplaced struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail"`
	// WeightLbs is the total weight of the unplaced units (unit weight × qty).
	// For a Rides() entry this weight is real cargo that is absent from
	// Placements, so Plan.UnmodeledWeightLbs folds it back into the exact gross.
	WeightLbs float64 `json:"weight_lbs,omitempty"`
}

// Blocking reports whether this entry means the truck cannot go as planned.
//
// Fail-closed by construction: everything except the one known-benign reason
// blocks, so a reason a future packer adds refuses a push until somebody
// classifies it deliberately.
func (u Unplaced) Blocking() bool { return u.Reason != ReasonNoGeometry }

// Rides reports whether the article still physically travels on the truck even
// though it is absent from the 3D plan. A dimensionless SKU does — there is
// nothing to position, but the box still goes on the deck. Cargo that did not
// FIT does not: it stays in the yard.
func (u Unplaced) Rides() bool { return u.Reason == ReasonNoGeometry }

// String renders the operator-facing line: "HANGER-26 ×120 (no geometry recorded)".
func (u Unplaced) String() string {
	return fmt.Sprintf("%s ×%d (%s)", u.SKU, u.Quantity, u.Detail)
}

// legacyUnplaced matches the flat string this field carried before it was
// typed: "SKU ×120 (truck full)".
var legacyUnplaced = regexp.MustCompile(`^(.*?) ×(\d+) \((.*)\)$`)

// UnmarshalJSON accepts both the typed object and the flat string the field
// used to be, because plans persist as JSONB: a run created before this type
// existed must still load rather than 500 the plan board. A legacy string is
// re-classified from its detail text, which is exactly the information the flat
// form carried.
func (u *Unplaced) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*u = parseLegacyUnplaced(s)
		return nil
	}
	type alias Unplaced // avoid recursing into this method
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*u = Unplaced(a)
	if u.Reason == "" {
		u.Reason = classifyUnplaced(u.Detail)
	}
	return nil
}

// parseLegacyUnplaced re-types a pre-typed-reason string entry.
func parseLegacyUnplaced(s string) Unplaced {
	m := legacyUnplaced.FindStringSubmatch(s)
	if m == nil {
		return Unplaced{SKU: s, Quantity: 1, Reason: classifyUnplaced(s), Detail: s}
	}
	qty, err := strconv.Atoi(m[2])
	if err != nil || qty <= 0 {
		qty = 1
	}
	return Unplaced{SKU: m[1], Quantity: qty, Reason: classifyUnplaced(m[3]), Detail: m[3]}
}

// classifyUnplaced maps legacy detail prose onto a reason code. Anything it
// does not recognise is treated as a capacity failure — the blocking default.
func classifyUnplaced(detail string) string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "no geometry"):
		return ReasonNoGeometry
	case strings.Contains(d, "volume"):
		return ReasonVolumeFull
	case strings.Contains(d, "too large"):
		return ReasonTooLarge
	case strings.Contains(d, "non-stackable"):
		return ReasonNotStackable
	default:
		return ReasonTruckFull
	}
}

// UnplacedStrings renders a slice for an operator-facing list.
func UnplacedStrings(us []Unplaced) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.String())
	}
	return out
}

// SplitUnplaced partitions a plan's unplaced entries into the cargo that did
// not fit (blocking — the customer ships short) and the articles that simply
// have no geometry to place (informational — they still ride).
func SplitUnplaced(us []Unplaced) (blocking, noGeometry []Unplaced) {
	for _, u := range us {
		if u.Blocking() {
			blocking = append(blocking, u)
		} else {
			noGeometry = append(noGeometry, u)
		}
	}
	return blocking, noGeometry
}
