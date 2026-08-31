// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package fleet

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrInvalidProfile is returned when a ProfileInput fails validation. Callers
// map it to HTTP 400.
var ErrInvalidProfile = errors.New("invalid vehicle profile")

// axleTypes is the closed set of axle types the load solver understands.
var axleTypes = map[string]bool{"STEER": true, "DRIVE": true, "TRAILER": true, "TAG": true}

// ValidationError carries every field problem found in one pass, so an operator
// fixing a form sees all of them at once instead of one per round trip.
type ValidationError struct {
	Fields []FieldError `json:"fields"`
}

// FieldError names one invalid field and why it was rejected.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return ErrInvalidProfile.Error() + " — " + strings.Join(parts, "; ")
}

// Is lets callers match with errors.Is(err, ErrInvalidProfile).
func (e *ValidationError) Is(target error) bool { return target == ErrInvalidProfile }

// Validate rejects a profile the load solver cannot reason about safely.
//
// This mirrors the browser form in app/src/pages/FleetProfiles.ts, which already
// refuses blank ratings. That check protected the UI path only: this endpoint is
// a whole-profile replace, so a direct PUT could still store a zero GVWR or a
// zero axle rating. The solver reads a non-positive rating as "unrated"
// (load.ratedStatus) and reports StatusUnknown rather than a confident PASS, so
// bad data degrades the verdict instead of falsifying it — but it degrades it
// silently, on a truck an operator believes is configured. Reject at the door.
//
// A missing tare is rejected for a sharper reason: gross weight is tare + cargo,
// so a zero tare understates gross and a genuinely overweight truck can pass the
// GVW gate, which is the one check that still hard-blocks a push.
func (in ProfileInput) Validate() error {
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{Field: field, Message: msg}) }

	if strings.TrimSpace(in.Name) == "" {
		add("name", "must not be blank")
	}
	for _, d := range []struct {
		field string
		val   float64
	}{
		{"bed_length_in", in.BedLengthIn},
		{"bed_width_in", in.BedWidthIn},
		{"bed_height_in", in.BedHeightIn},
	} {
		if d.val <= 0 {
			add(d.field, "must be greater than zero")
		}
	}
	if in.GVWRLbs <= 0 {
		add("gvwr_lbs", "must be greater than zero — the solver reads a non-positive GVWR as unrated and cannot judge gross weight")
	}
	if in.TareWeightLbs <= 0 {
		add("tare_weight_lbs", "must be greater than zero — gross weight is tare plus cargo, so a zero tare understates gross and can let an overweight truck pass the GVW gate")
	}

	if len(in.Axles) == 0 {
		add("axles", "at least one axle is required")
		return finish(errs)
	}

	steers := 0
	seenNumbers := map[int]bool{}
	for i, a := range in.Axles {
		p := fmt.Sprintf("axles[%d]", i)
		if a.AxleNumber <= 0 {
			add(p+".axle_number", "must be greater than zero")
		} else if seenNumbers[a.AxleNumber] {
			add(p+".axle_number", fmt.Sprintf("duplicate axle number %d", a.AxleNumber))
		} else {
			seenNumbers[a.AxleNumber] = true
		}
		if a.MaxWeightLbs <= 0 {
			add(p+".max_weight_lbs", "must be greater than zero — a non-positive rating is read as unrated")
		}
		if a.PositionFromFrontIn < 0 {
			add(p+".position_from_front_in", "must not be negative — positions are measured rearward from the front of the bed")
		}
		t := strings.ToUpper(strings.TrimSpace(a.AxleType))
		if !axleTypes[t] {
			add(p+".axle_type", "must be one of STEER, DRIVE, TRAILER, TAG")
		}
		if t == "STEER" {
			steers++
		}
	}
	if steers == 0 {
		add("axles", "exactly one axle must be of type STEER — the per-axle model uses it as the datum")
	} else if steers > 1 {
		add("axles", fmt.Sprintf("found %d STEER axles — exactly one is required", steers))
	}

	// Positions must increase with axle number: an axle ordered behind another
	// but positioned ahead of it makes the lever arms meaningless.
	ordered := append([]AxleInput(nil), in.Axles...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].AxleNumber < ordered[j].AxleNumber })
	for i := 1; i < len(ordered); i++ {
		if ordered[i].PositionFromFrontIn <= ordered[i-1].PositionFromFrontIn {
			add("axles", fmt.Sprintf(
				"axle %d is at %.1f in but axle %d is at %.1f in — positions must increase with axle number",
				ordered[i].AxleNumber, ordered[i].PositionFromFrontIn,
				ordered[i-1].AxleNumber, ordered[i-1].PositionFromFrontIn))
			break
		}
	}

	return finish(errs)
}

func finish(errs []FieldError) error {
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Fields: errs}
}
