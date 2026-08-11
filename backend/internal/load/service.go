// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"context"
	"errors"
	"fmt"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/fleet"
)

// profileProvider supplies vehicle profiles (satisfied by *fleet.Service).
type profileProvider interface {
	GetProfile(ctx context.Context, gableVehicleID string) (*fleet.Profile, error)
}

// OptimizeRequest is the input to a load-optimization run. Items are resolved
// to dimensions/weight by the caller (from catalog + GableLBM order lines).
type OptimizeRequest struct {
	VehicleID  string  `json:"vehicle_id"`
	RouteID    *string `json:"route_id,omitempty"`
	DeliveryID *string `json:"delivery_id,omitempty"`
	Items      []Item  `json:"items"`
}

// Service orchestrates load optimization: fetch profile → solve → persist.
type Service struct {
	repo     *Repository
	profiles profileProvider
	solver   Solver
	// securement policy inputs (T1-5/T2-7) applied to the solved vehicle.
	securementJurisdiction string
	anchorSpacingIn        float64
}

func NewService(repo *Repository, profiles profileProvider, solver Solver, securementJurisdiction string, anchorSpacingIn float64) *Service {
	return &Service{
		repo:                   repo,
		profiles:               profiles,
		solver:                 solver,
		securementJurisdiction: securementJurisdiction,
		anchorSpacingIn:        anchorSpacingIn,
	}
}

// Optimize computes and persists a load plan for the request.
func (s *Service) Optimize(ctx context.Context, req OptimizeRequest) (*Plan, error) {
	if req.VehicleID == "" {
		return nil, fmt.Errorf("vehicle_id is required")
	}

	profile, err := s.profiles.GetProfile(ctx, req.VehicleID)
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, fmt.Errorf("no fleet profile for vehicle %s: %w", req.VehicleID, err)
	}
	if err != nil {
		return nil, fmt.Errorf("load vehicle profile: %w", err)
	}

	vehicle := toSolverVehicle(profile)
	vehicle.SecurementJurisdiction = s.securementJurisdiction
	vehicle.AnchorSpacingIn = s.anchorSpacingIn
	plan := s.solver.Solve(vehicle, req.Items)
	plan.GableRouteID = req.RouteID
	plan.GableDeliveryID = req.DeliveryID

	if err := s.repo.Save(ctx, &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// Get returns a stored plan by id.
func (s *Service) Get(ctx context.Context, id string) (*Plan, error) {
	return s.repo.Get(ctx, id)
}

func toSolverVehicle(p *fleet.Profile) Vehicle {
	v := Vehicle{
		GableVehicleID: p.GableVehicleID,
		BedLengthIn:    p.BedLengthIn,
		BedWidthIn:     p.BedWidthIn,
		BedHeightIn:    p.BedHeightIn,
		GVWRLbs:        p.GVWRLbs,
		TareWeightLbs:  p.TareWeightLbs,
	}
	for _, a := range p.Axles {
		v.Axles = append(v.Axles, Axle{
			AxleNumber:          a.AxleNumber,
			MaxWeightLbs:        a.MaxWeightLbs,
			PositionFromFrontIn: a.PositionFromFrontIn,
			AxleType:            a.AxleType,
		})
	}
	return v
}
