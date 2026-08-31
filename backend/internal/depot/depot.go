// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package depot holds the ONE ladder that decides where a run is rooted.
//
// A route's origin is not a cosmetic field: it is the first and last leg of
// every sequence, and it propagates into distance, duration, ETA and the
// shift-feasibility check. Rooting a run at the wrong yard produces a plan that
// looks entirely reasonable and is wrong by however far apart the two yards
// are.
//
// This package exists because that decision was being made twice, in two
// modules of the same binary, with two different answers: the workflow ingest
// ran REQUEST -> BRANCH -> CONFIG -> CENTROID, while the routing endpoint ran
// REQUEST -> CENTROID and ignored the branch id its own caller had handed it.
// A second copy of a ladder is a ladder that will drift, so there is exactly
// one here and both callers use it.
//
// Resolve is a pure function: no I/O, no clock, no logging. Callers fetch the
// branch list (however they get it), hand it in, and act on what comes back.
package depot

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// Depot origin sources. They are recorded on a plan (or logged) so the UI and
// support can see where a run's routing origin actually came from. The source
// always names what happened — never what was hoped for — because it is read
// by someone trying to explain a route to a dispatcher.
const (
	SourceRequest  = "REQUEST"  // supplied on the request itself
	SourceBranch   = "BRANCH"   // the GableLBM yard this run ships from
	SourceConfig   = "CONFIG"   // this install's DEPOT_LAT/DEPOT_LNG
	SourceCentroid = "CENTROID" // centroid of the run's routable stops
	SourceNone     = "NONE"     // nothing to root on (no depot, no geocoded stop)
)

// Point is one routable stop's coordinate. Only stops that can actually be
// driven to belong here: an ungeocoded order must not drag the centroid.
type Point struct {
	Lat float64
	Lng float64
}

// Input is everything the ladder needs. Every rung is optional; the ladder
// simply falls through to the next one it can satisfy.
type Input struct {
	// RequestLat/RequestLng are this run's explicit override — the operator's
	// own answer, which nothing outranks. Both must be present: a half-supplied
	// depot is ignored, never half-applied.
	RequestLat *float64
	RequestLng *float64

	// BranchIDs are the distinct GableLBM yards this run wants to leave from.
	// Callers that have one branch id (an explicit branch on the request) pass
	// one; callers that derive it from the run's orders pass the distinct set
	// they found. Empty means "no branch was named", which is not an error and
	// draws no note. More than one is ambiguous: the run is rooted further down
	// the ladder and told why, because silently picking one of two yards roots
	// half the day's stops in the wrong place.
	BranchIDs []string

	// Branches is GableLBM's active branch list, used to turn a branch id into
	// a coordinate. A caller that could not fetch it passes nil; the ladder
	// then reports the branch as unknown, and the caller — which is the only
	// one that knows a lookup failed — should say so instead (see
	// FallbackPhrase).
	Branches []gable.Location

	// ConfigLat/ConfigLng are this install's configured yard (DEPOT_LAT/
	// DEPOT_LNG): one coordinate for the whole install, which is the wrong
	// answer for a dealer shipping from several yards and so sits below BRANCH.
	// Both must be present, for the same reason as the request pair.
	ConfigLat *float64
	ConfigLng *float64

	// Stops are the run's routable stop coordinates, used for the centroid.
	Stops []Point
}

// Resolve picks a run's routing origin, in strict precedence:
//
//  1. the request (this run's explicit override);
//  2. the GableLBM branch this run ships from — the yard the load actually
//     leaves from;
//  3. this install's configured depot (DEPOT_LAT/DEPOT_LNG);
//  4. the centroid of the run's routable stops.
//
// There is deliberately no built-in coordinate at any level: a hardcoded
// default roots every other dealer's routes at one customer's yard. When
// nothing at all is available it reports SourceNone and (0,0) — there are no
// routable stops in that case, so the origin is unused, but the caller can see
// it and warn.
//
// note is non-empty only when the branch rung was possible but declined. It
// says why, and then names the fallback actually used, in a sentence written
// for an operator: it is the difference between "the route starts in Austin"
// and "the route starts in Austin because your Plano yard has never been
// geocoded".
func Resolve(in Input) (lat, lng float64, source, note string) {
	if in.RequestLat != nil && in.RequestLng != nil {
		return *in.RequestLat, *in.RequestLng, SourceRequest, ""
	}
	bLat, bLng, ok, why := resolveBranch(in.BranchIDs, in.Branches)
	if ok {
		return bLat, bLng, SourceBranch, ""
	}
	lat, lng, source = resolveFallback(in)
	if why != "" {
		note = why + "; " + FallbackPhrase(source)
	}
	return lat, lng, source, note
}

// resolveFallback is everything below the branch rung: the configured yard,
// then the centroid of the run's routable stops, then nothing.
func resolveFallback(in Input) (lat, lng float64, source string) {
	if in.ConfigLat != nil && in.ConfigLng != nil {
		return *in.ConfigLat, *in.ConfigLng, SourceConfig
	}
	if len(in.Stops) > 0 {
		var sumLat, sumLng float64
		for _, s := range in.Stops {
			sumLat += s.Lat
			sumLng += s.Lng
		}
		n := float64(len(in.Stops))
		return round6(sumLat / n), round6(sumLng / n), SourceCentroid
	}
	return 0, 0, SourceNone
}

// resolveBranch answers "does this run leave from one yard, and do we know
// where that yard is?".
//
// ok is true only when the answer is unambiguous. Every other outcome returns
// ok=false with a reason, because the alternative — picking one yard out of
// several, or rooting at a null coordinate read as 0,0 — produces a plan that
// looks right and is wrong. Splitting a run per branch is Phase 2; until then
// the honest move is to fall back and say so.
//
// A run that names no branch at all (a GableLBM that predates orders.branch_id,
// or a caller that simply did not supply one) is not an error and gets no note:
// ok=false, why="".
func resolveBranch(ids []string, branches []gable.Location) (lat, lng float64, ok bool, why string) {
	byID := make(map[string]gable.Location, len(branches))
	for _, b := range branches {
		byID[b.ID] = b
	}

	distinct := make([]string, 0, 2)
	seen := make(map[string]bool, 2)
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		distinct = append(distinct, id)
	}

	switch len(distinct) {
	case 0:
		return 0, 0, false, ""
	case 1:
		// The only case that can succeed; fall through to the checks below.
	default:
		labels := make([]string, 0, len(distinct))
		for _, id := range distinct {
			labels = append(labels, describeBranch(byID, id))
		}
		sort.Strings(labels)
		return 0, 0, false, fmt.Sprintf(
			"this run's orders ship from %d different branches (%s), so no single yard is its origin — per-branch plan splitting is not supported yet",
			len(labels), strings.Join(labels, ", "))
	}

	id := distinct[0]
	b, found := byID[id]
	if !found {
		// Inactive or otherwise absent from GableLBM's branch list.
		return 0, 0, false, fmt.Sprintf("branch %s is not in GableLBM's active branch list", id)
	}
	if b.Latitude == nil || b.Longitude == nil {
		return 0, 0, false, fmt.Sprintf("branch %s has no coordinates in GableLBM — it has never been geocoded", describeBranch(byID, id))
	}
	if !ValidCoords(*b.Latitude, *b.Longitude) {
		return 0, 0, false, fmt.Sprintf("branch %s has out-of-range coordinates (%v, %v) in GableLBM",
			describeBranch(byID, id), *b.Latitude, *b.Longitude)
	}
	return *b.Latitude, *b.Longitude, true, ""
}

// describeBranch renders a branch for an operator: its name when GableLBM knows
// it, and always its id, because the id is what support can search on.
func describeBranch(byID map[string]gable.Location, id string) string {
	if b, ok := byID[id]; ok && b.Name != "" {
		return fmt.Sprintf("%q (%s)", b.Name, id)
	}
	return id
}

// FallbackPhrase completes a note by naming where the run was rooted once the
// branch rung declined. It is exported because a caller that failed to FETCH
// the branch list must rewrite the first half of the note (the ladder saw an
// unknown branch; the caller knows the lookup itself failed) while keeping this
// second half identical.
func FallbackPhrase(source string) string {
	switch source {
	case SourceConfig:
		return "rooted at this install's configured depot (DEPOT_LAT/DEPOT_LNG) instead"
	case SourceCentroid:
		return "rooted at the centroid of the day's stops instead"
	default:
		return "there is nothing else to root on either: no depot is configured and no order on this date is geocoded"
	}
}

// ValidCoords applies the same range rule the boot-time depot ladder applies
// (see internal/config.loadDepot). A branch coordinate arrives as DATA over the
// wire rather than as deployment configuration, so a bad one cannot be a boot
// failure — but it must not be treated more loosely either: it is refused and
// recorded, never half-applied and never silently taken as 0,0.
func ValidCoords(lat, lng float64) bool {
	if math.IsNaN(lat) || math.IsNaN(lng) || math.IsInf(lat, 0) || math.IsInf(lng, 0) {
		return false
	}
	return lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180
}

func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }
