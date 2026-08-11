// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package compliance

import (
	"context"
	"fmt"
	"math"
)

const defaultBufferMiles = 0.5

// Service holds compliance business logic: restricted-point CRUD + route checks.
type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

// ListPoints returns all restricted points.
func (s *Service) ListPoints(ctx context.Context) ([]RestrictedPoint, error) {
	return s.repo.List(ctx)
}

// CreatePoint adds a restricted point.
func (s *Service) CreatePoint(ctx context.Context, in RestrictedPointInput) (*RestrictedPoint, error) {
	return s.repo.Create(ctx, in)
}

// UpdatePoint edits a restricted point.
func (s *Service) UpdatePoint(ctx context.Context, id string, in RestrictedPointInput) (*RestrictedPoint, error) {
	return s.repo.Update(ctx, id, in)
}

// CheckRoute flags restricted points within the buffer of the route polyline
// whose limits the load violates. Distance is the minimum haversine distance
// from the point to any route vertex (MVP simplification of segment projection).
func (s *Service) CheckRoute(ctx context.Context, req RouteCheckRequest) (*RouteCheckResult, error) {
	points, err := s.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("load restricted points: %w", err)
	}

	buffer := req.BufferMiles
	if buffer <= 0 {
		buffer = defaultBufferMiles
	}

	result := &RouteCheckResult{Status: "PASS", Flags: []Flag{}}
	worst := 0 // 0 PASS, 1 WARN, 2 FAIL

	for _, p := range points {
		dist := minDistanceToRoute(p, req.Route)
		if dist > buffer {
			continue
		}
		violation, severity := evaluate(p, req.Load)
		if severity == "" {
			continue // within buffer but load complies
		}
		result.Flags = append(result.Flags, Flag{
			Point:      p,
			DistanceMi: round2(dist),
			Violation:  violation,
			Severity:   severity,
		})
		if rank := sevRank(severity); rank > worst {
			worst = rank
		}
	}

	result.Status = rankSev(worst)
	return result, nil
}

// evaluate returns a violation reason + severity if the load breaches the
// point's limits, else empty strings.
func evaluate(p RestrictedPoint, load LoadProfile) (string, string) {
	if p.MaxGrossWeightLbs != nil && load.GrossWeightLbs > *p.MaxGrossWeightLbs {
		return fmt.Sprintf("gross weight %d lbs exceeds limit %d lbs", load.GrossWeightLbs, *p.MaxGrossWeightLbs), "FAIL"
	}
	if p.MaxAxleWeightLbs != nil && load.MaxAxleLbs > *p.MaxAxleWeightLbs {
		return fmt.Sprintf("axle weight %d lbs exceeds limit %d lbs", load.MaxAxleLbs, *p.MaxAxleWeightLbs), "FAIL"
	}
	if p.MaxHeightIn != nil && load.HeightIn > *p.MaxHeightIn {
		return fmt.Sprintf("load height %.1f in exceeds clearance %.1f in", load.HeightIn, *p.MaxHeightIn), "FAIL"
	}
	if p.RestrictionType == "SEASONAL" {
		return "seasonal restriction on route — verify before dispatch", "WARN"
	}
	return "", ""
}

// minDistanceToRoute returns the smallest haversine distance (miles) from the
// point to any vertex of the route.
func minDistanceToRoute(p RestrictedPoint, route []RoutePoint) float64 {
	if len(route) == 0 {
		return math.Inf(1)
	}
	min := math.Inf(1)
	for _, rp := range route {
		d := haversineMiles(p.Lat, p.Lng, rp.Lat, rp.Lng)
		if d < min {
			min = d
		}
	}
	return min
}

// haversineMiles returns great-circle distance in miles between two coordinates.
func haversineMiles(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusMi = 3958.8
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMi * c
}

func sevRank(s string) int {
	switch s {
	case "FAIL":
		return 2
	case "WARN":
		return 1
	default:
		return 0
	}
}

func rankSev(r int) string {
	switch r {
	case 2:
		return "FAIL"
	case 1:
		return "WARN"
	default:
		return "PASS"
	}
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
