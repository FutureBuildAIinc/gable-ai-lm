// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

// Step 5: route flag review with automatic resolution.
//
// Every truck's route polyline (depot → stops, with each leg interpolated so
// restrictions BETWEEN stops are caught, not just at them) is checked against
// the restricted-point registry. When a load violates a restriction the
// reviewer resolves it automatically, in order of preference:
//
//	1. REROUTE     — insert a detour waypoint that steers the leg around the
//	                 restriction (possible when the restriction is not at a
//	                 stop itself).
//	2. LOAD_ADJUST — height violations: re-pack the truck with a capped load
//	                 height below the lowest flagged clearance.
//	                 weight violations: move the heaviest stop to another truck
//	                 with spare capacity, then re-pack and re-check both.
//	3. MANUAL_REVIEW — anything still failing is surfaced for the dispatcher.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/compliance"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/routing"
)

const (
	reviewBufferMiles  = 0.5 // must match compliance.defaultBufferMiles
	legSampleStepMiles = 0.4 // polyline interpolation density
	maxLegSamples      = 16  // cap per sub-segment
	detourClearMiles   = 1.4 // how far past the buffer a detour swings
	heightSafetyIn     = 2.0 // clearance margin when capping load height
)

// detour is an AI-inserted waypoint on a specific route leg (0 = depot→stop 1).
type detour struct {
	Leg   int                   `json:"leg"`
	Point compliance.RoutePoint `json:"point"`
}

// Review runs the restricted-point check + auto-resolution across every truck.
func (s *Service) Review(ctx context.Context, id string) (*Plan, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(p.Loads) == 0 {
		return nil, fmt.Errorf("no truck loads to review — run assign + pack first")
	}
	for _, l := range p.Loads {
		if l.LoadPlan == nil {
			return nil, fmt.Errorf("truck %s is not packed yet — run pack first", l.VehicleName)
		}
	}

	vehicles, err := s.gable.ListVehicles(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch vehicles: %w", err)
	}
	vehiclesByID := make(map[string]gable.Vehicle, len(vehicles))
	for _, v := range vehicles {
		vehiclesByID[v.ID] = v
	}

	for i := range p.Loads {
		if err := s.reviewLoad(ctx, p, &p.Loads[i], vehiclesByID); err != nil {
			return nil, err
		}
	}

	// Cross-truck weight rebalance for loads still failing on weight.
	if err := s.rebalanceOverweight(ctx, p, vehiclesByID); err != nil {
		return nil, err
	}

	p.Status = StatusReviewed
	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// reviewLoad checks one truck and applies reroute + height-cap resolutions.
func (s *Service) reviewLoad(ctx context.Context, p *Plan, l *TruckLoad, vehiclesByID map[string]gable.Vehicle) error {
	review := &ComplianceReview{Actions: []ComplianceAction{}, Flags: []compliance.Flag{}}
	var detours []detour

	res, err := s.checkOnce(ctx, p, l, detours, review)
	if err != nil {
		return err
	}

	// 1. REROUTE — detour around every avoidable FAIL point.
	if res.Status == "FAIL" {
		var avoided []string
		for _, f := range res.Flags {
			if f.Severity != "FAIL" {
				continue
			}
			if pointAtStop(f.Point, p, l) {
				continue // the restriction sits at a delivery point — cannot route around it
			}
			if d, ok := computeDetour(f.Point, p, l); ok {
				detours = append(detours, d)
				avoided = append(avoided, f.Point.Name)
			}
		}
		if len(avoided) > 0 {
			res2, err := s.checkOnce(ctx, p, l, detours, review)
			if err != nil {
				return err
			}
			review.Actions = append(review.Actions, ComplianceAction{
				Type: "REROUTE",
				Description: fmt.Sprintf("Inserted %d detour waypoint(s) to route around: %s",
					len(detours), strings.Join(avoided, ", ")),
				Resolved: res2.Status != "FAIL",
			})
			res = res2
		}
	}

	// 2. LOAD_ADJUST (height) — re-pack under the lowest flagged clearance.
	if res.Status == "FAIL" {
		minClearance := math.Inf(1)
		var lowPoint string
		for _, f := range res.Flags {
			if f.Severity == "FAIL" && f.Point.MaxHeightIn != nil && *f.Point.MaxHeightIn < minClearance {
				minClearance = *f.Point.MaxHeightIn
				lowPoint = f.Point.Name
			}
		}
		if !math.IsInf(minClearance, 1) {
			capIn := minClearance - defaultDeckHeightIn - heightSafetyIn
			if capIn >= 12 { // anything lower than one foot of cargo is not packable
				if err := s.packLoad(ctx, p, l, vehiclesByID, capIn); err != nil {
					return err
				}
				res2, err := s.checkOnce(ctx, p, l, detours, review)
				if err != nil {
					return err
				}
				review.Actions = append(review.Actions, ComplianceAction{
					Type: "LOAD_ADJUST",
					Description: fmt.Sprintf("Re-packed load below %.0f in to clear %s (%.0f in clearance)",
						capIn, lowPoint, minClearance),
					Resolved: res2.Status != "FAIL",
				})
				res = res2
			}
		}
	}

	review.Status = res.Status
	review.Flags = res.Flags
	for _, d := range detours {
		review.Detours = append(review.Detours, d.Point)
	}
	applyCapacityVerdict(review, *l)
	l.Compliance = review
	return nil
}

// applyCapacityVerdict folds the truck's OWN load solve into the review that
// the dispatcher sees and that Push gates on. The restricted-point check only
// knows about bridges and overpasses; a truck packed over its GVWR or axle
// rating (or one whose rating could not be verified), or one carrying less than
// the order because pieces did not fit, is just as undispatchable even on a
// route with no restrictions at all.
//
// FAIL, not WARN: these are hard refusals at Push, so Review must say so rather
// than show a green badge the dispatcher will trust. It adds no compliance
// Flag, so it never triggers the restricted-point weight rebalancer — this is a
// packing problem, not a route problem.
func applyCapacityVerdict(review *ComplianceReview, l TruckLoad) {
	reasons := blockingCapacityReasons(l)
	if len(reasons) == 0 {
		// A near-rating (WARN) load is legal but worth surfacing.
		if l.LoadPlan != nil && strings.EqualFold(strings.TrimSpace(l.LoadPlan.GVWStatus), "WARN") && review.Status == "PASS" {
			review.Status = "WARN"
		}
		return
	}
	review.Status = "FAIL"
	review.Actions = append(review.Actions, ComplianceAction{
		Type:        "MANUAL_REVIEW",
		Description: "Load capacity not cleared: " + strings.Join(reasons, "; "),
		Resolved:    false,
	})
}

// checkOnce runs one compliance check for the load's current packing + detours
// and records the checked profile on the review.
func (s *Service) checkOnce(ctx context.Context, p *Plan, l *TruckLoad, detours []detour, review *ComplianceReview) (*compliance.RouteCheckResult, error) {
	maxAxle := int64(0)
	for _, a := range l.LoadPlan.AxleLoads {
		if a.WeightLbs > maxAxle {
			maxAxle = a.WeightLbs
		}
	}
	profile := compliance.LoadProfile{
		GrossWeightLbs: l.LoadPlan.TotalWeightLbs,
		MaxAxleLbs:     maxAxle,
		HeightIn:       defaultDeckHeightIn + l.LoadPlan.MaxLoadHeightIn,
	}
	review.CheckedGrossLbs = profile.GrossWeightLbs
	review.CheckedMaxAxleLbs = profile.MaxAxleLbs
	review.CheckedHeightIn = round2(profile.HeightIn)

	res, err := s.checker.CheckRoute(ctx, compliance.RouteCheckRequest{
		Route:       buildPolyline(p.DepotLat, p.DepotLng, l.Stops, detours),
		Load:        profile,
		BufferMiles: reviewBufferMiles,
	})
	if err != nil {
		return nil, fmt.Errorf("compliance check (truck %s): %w", l.VehicleName, err)
	}
	return res, nil
}

// rebalanceOverweight moves the heaviest stop off any truck still failing on a
// weight restriction onto another truck with spare capacity, then re-packs and
// re-reviews both.
func (s *Service) rebalanceOverweight(ctx context.Context, p *Plan, vehiclesByID map[string]gable.Vehicle) error {
	for i := range p.Loads {
		src := &p.Loads[i]
		if src.Compliance == nil || src.Compliance.Status != "FAIL" || !hasWeightFail(src.Compliance) || len(src.Stops) < 2 {
			continue
		}

		// Heaviest stop on the failing truck.
		hi := 0
		for k, st := range src.Stops {
			if st.WeightLbs > src.Stops[hi].WeightLbs {
				hi = k
			}
		}
		moved := src.Stops[hi]

		dstIdx := -1
		for j := range p.Loads {
			if j == i {
				continue
			}
			cand := &p.Loads[j]
			if cand.TotalWeightLbs+moved.WeightLbs <= float64(cand.CapacityWeightLbs) {
				dstIdx = j
				break
			}
		}
		if dstIdx < 0 {
			// No active truck has room — activate an idle vehicle from the fleet.
			if idle := s.activateIdleTruck(ctx, p, moved.WeightLbs, vehiclesByID); idle >= 0 {
				dstIdx = idle
				src = &p.Loads[i] // p.Loads may have been reallocated by append
			}
		}
		if dstIdx < 0 {
			src.Compliance.Actions = append(src.Compliance.Actions, ComplianceAction{
				Type:        "MANUAL_REVIEW",
				Description: fmt.Sprintf("Weight restriction unresolved — no truck has %.0f lb spare capacity", moved.WeightLbs),
				Resolved:    false,
			})
			continue
		}
		dst := &p.Loads[dstIdx]

		// Move the stop and re-sequence both trucks.
		src.Stops = append(src.Stops[:hi], src.Stops[hi+1:]...)
		dst.Stops = append(dst.Stops, moved)
		for _, l := range []*TruckLoad{src, dst} {
			resequenceOptimal(p, l)
			if err := s.packLoad(ctx, p, l, vehiclesByID, 0); err != nil {
				return err
			}
			if err := s.reviewLoad(ctx, p, l, vehiclesByID); err != nil {
				return err
			}
		}
		src.Compliance.Actions = append(src.Compliance.Actions, ComplianceAction{
			Type: "LOAD_ADJUST",
			Description: fmt.Sprintf("Moved %s (%.0f lb) to %s to clear a weight restriction",
				stopLabel(moved), moved.WeightLbs, dst.VehicleName),
			Resolved: src.Compliance.Status != "FAIL",
		})
	}
	return nil
}

// activateIdleTruck pulls an unused fleet vehicle (with an unused ACTIVE
// driver) into the plan so a weight rebalance has somewhere to move cargo.
// Returns the new load's index in p.Loads, or -1.
func (s *Service) activateIdleTruck(ctx context.Context, p *Plan, neededLbs float64, vehiclesByID map[string]gable.Vehicle) int {
	active := map[string]bool{}
	usedDrivers := map[string]bool{}
	for _, l := range p.Loads {
		active[l.VehicleID] = true
		usedDrivers[l.DriverID] = true
	}

	var pick *gable.Vehicle
	for id := range vehiclesByID {
		v := vehiclesByID[id]
		if active[v.ID] || v.CapacityWeightLbs == nil {
			continue
		}
		if float64(*v.CapacityWeightLbs)*cargoUtilizationCap < neededLbs {
			continue
		}
		if pick == nil || *v.CapacityWeightLbs < *pick.CapacityWeightLbs ||
			(*v.CapacityWeightLbs == *pick.CapacityWeightLbs && v.ID < pick.ID) {
			vv := v
			pick = &vv // smallest truck that fits (deterministic)
		}
	}
	if pick == nil {
		return -1
	}

	nl := TruckLoad{
		VehicleID:         pick.ID,
		VehicleName:       pick.Name,
		CapacityWeightLbs: *pick.CapacityWeightLbs,
		Stops:             []Stop{},
	}
	if drivers, err := s.gable.ListDrivers(ctx); err == nil {
		sort.SliceStable(drivers, func(i, j int) bool { return drivers[i].ID < drivers[j].ID })
		for _, d := range drivers {
			if !usedDrivers[d.ID] && (d.Status == "" || d.Status == "ACTIVE") {
				nl.DriverID = d.ID
				nl.DriverName = d.Name
				break
			}
		}
	}
	p.Loads = append(p.Loads, nl)
	return len(p.Loads) - 1
}

func stopLabel(st Stop) string {
	if st.CustomerName != "" {
		return st.CustomerName
	}
	return "order " + st.OrderID
}

func hasWeightFail(r *ComplianceReview) bool {
	for _, f := range r.Flags {
		if f.Severity == "FAIL" && (f.Point.MaxGrossWeightLbs != nil || f.Point.MaxAxleWeightLbs != nil) {
			return true
		}
	}
	return false
}

// --- polyline geometry --------------------------------------------------------

// buildPolyline produces the route polyline: depot → stops in sequence, each
// leg interpolated (so restrictions between stops register), with any detour
// waypoints spliced into their leg.
func buildPolyline(depotLat, depotLng float64, stops []Stop, detours []detour) []compliance.RoutePoint {
	nodes := make([]compliance.RoutePoint, 0, len(stops)+1)
	nodes = append(nodes, compliance.RoutePoint{Lat: depotLat, Lng: depotLng})
	for _, st := range stops {
		nodes = append(nodes, compliance.RoutePoint{Lat: st.Lat, Lng: st.Lng})
	}

	byLeg := map[int][]compliance.RoutePoint{}
	for _, d := range detours {
		byLeg[d.Leg] = append(byLeg[d.Leg], d.Point)
	}

	var poly []compliance.RoutePoint
	for i := 0; i < len(nodes)-1; i++ {
		waypoints := append([]compliance.RoutePoint{nodes[i]}, byLeg[i]...)
		waypoints = append(waypoints, nodes[i+1])
		for w := 0; w < len(waypoints)-1; w++ {
			poly = append(poly, interpolate(waypoints[w], waypoints[w+1])...)
		}
	}
	poly = append(poly, nodes[len(nodes)-1])
	return poly
}

// interpolate returns a (start-inclusive, end-exclusive) sampled segment.
func interpolate(a, b compliance.RoutePoint) []compliance.RoutePoint {
	dist := routing.HaversineMiles(a.Lat, a.Lng, b.Lat, b.Lng)
	n := int(dist / legSampleStepMiles)
	if n > maxLegSamples {
		n = maxLegSamples
	}
	out := []compliance.RoutePoint{a}
	for k := 1; k <= n; k++ {
		t := float64(k) / float64(n+1)
		out = append(out, compliance.RoutePoint{
			Lat: a.Lat + (b.Lat-a.Lat)*t,
			Lng: a.Lng + (b.Lng-a.Lng)*t,
		})
	}
	return out
}

// pointAtStop reports whether the restriction sits at (within the buffer of)
// the depot or one of the truck's delivery points — i.e. unavoidable by reroute.
func pointAtStop(pt compliance.RestrictedPoint, p *Plan, l *TruckLoad) bool {
	if routing.HaversineMiles(pt.Lat, pt.Lng, p.DepotLat, p.DepotLng) <= reviewBufferMiles {
		return true
	}
	for _, st := range l.Stops {
		if routing.HaversineMiles(pt.Lat, pt.Lng, st.Lat, st.Lng) <= reviewBufferMiles {
			return true
		}
	}
	return false
}

// computeDetour places a waypoint that swings the offending leg around the
// restricted point: it finds the leg passing nearest the point and offsets a
// waypoint directly away from the point, past the check buffer.
func computeDetour(pt compliance.RestrictedPoint, p *Plan, l *TruckLoad) (detour, bool) {
	nodes := make([]compliance.RoutePoint, 0, len(l.Stops)+1)
	nodes = append(nodes, compliance.RoutePoint{Lat: p.DepotLat, Lng: p.DepotLng})
	for _, st := range l.Stops {
		nodes = append(nodes, compliance.RoutePoint{Lat: st.Lat, Lng: st.Lng})
	}

	bestLeg, bestDist := -1, math.Inf(1)
	var nearest compliance.RoutePoint
	for i := 0; i < len(nodes)-1; i++ {
		for _, sp := range interpolate(nodes[i], nodes[i+1]) {
			if d := routing.HaversineMiles(pt.Lat, pt.Lng, sp.Lat, sp.Lng); d < bestDist {
				bestDist = d
				bestLeg = i
				nearest = sp
			}
		}
	}
	if bestLeg < 0 {
		return detour{}, false
	}

	// Offset direction: from the restricted point through the nearest route
	// sample, in local mile-space (lng scaled by cos(lat)).
	cosLat := math.Cos(pt.Lat * math.Pi / 180)
	dx := (nearest.Lng - pt.Lng) * 69.0 * cosLat
	dy := (nearest.Lat - pt.Lat) * 69.0
	norm := math.Hypot(dx, dy)
	if norm < 1e-6 {
		// Route passes directly over the point — push perpendicular to the leg.
		a, b := nodes[bestLeg], nodes[bestLeg+1]
		lx := (b.Lng - a.Lng) * 69.0 * cosLat
		ly := (b.Lat - a.Lat) * 69.0
		ln := math.Hypot(lx, ly)
		if ln < 1e-6 {
			return detour{}, false
		}
		dx, dy = -ly/ln, lx/ln
		norm = 1
	}
	offset := reviewBufferMiles + detourClearMiles
	return detour{
		Leg: bestLeg,
		Point: compliance.RoutePoint{
			Lat: pt.Lat + (dy/norm)*offset/69.0,
			Lng: pt.Lng + (dx/norm)*offset/(69.0*cosLat),
		},
	}, true
}

// resequenceOptimal re-runs the route optimizer over a load's current stops,
// keeping any priority (deliver-first) stops pinned to the front (T2-1).
func resequenceOptimal(p *Plan, l *TruckLoad) {
	rstops := make([]routing.Stop, 0, len(l.Stops))
	for _, st := range l.Stops {
		rstops = append(rstops, routing.Stop{
			OrderID:   st.OrderID,
			Lat:       st.Lat,
			Lng:       st.Lng,
			Address:   st.Address,
			WeightLbs: st.WeightLbs,
		})
	}
	ordered, dist, dur := sequenceWithPriority(p.DepotLat, p.DepotLng, rstops, prioritySet(p))
	byOrder := orderIndex(p)
	l.Stops = make([]Stop, 0, len(ordered))
	for _, st := range ordered {
		l.Stops = append(l.Stops, toWorkflowStop(st, byOrder))
	}
	l.TotalDistanceMi = dist
	l.TotalDurationMin = dur
	l.TotalWeightLbs = 0
	for _, st := range l.Stops {
		l.TotalWeightLbs += st.WeightLbs
	}
	l.TotalWeightLbs = round2(l.TotalWeightLbs)
}
