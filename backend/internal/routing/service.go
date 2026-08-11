// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"context"
	"fmt"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// orderSource fetches confirmed orders for a date (satisfied by *gable.Client).
type orderSource interface {
	ListOrdersForDate(ctx context.Context, date string) ([]gable.Order, error)
}

// vehicleSource fetches the fleet (with capacities) for CVRP assignment
// (satisfied by *gable.Client).
type vehicleSource interface {
	ListVehicles(ctx context.Context) ([]gable.Vehicle, error)
}

// driverSource fetches the fleet's drivers; GableLBM requires a valid driver id
// on delivery-route write-back (satisfied by *gable.Client).
type driverSource interface {
	ListDrivers(ctx context.Context) ([]gable.Driver, error)
}

// routeSink writes an approved route back to GableLBM (satisfied by *gable.Client).
type routeSink interface {
	PushDeliveryRoute(ctx context.Context, route gable.DeliveryRoute) error
}

// Service orchestrates route planning and write-back.
type Service struct {
	repo     *Repository
	orders   orderSource
	vehicles vehicleSource
	drivers  driverSource
	sink     routeSink
}

func NewService(repo *Repository, orders orderSource, vehicles vehicleSource, drivers driverSource, sink routeSink) *Service {
	return &Service{repo: repo, orders: orders, vehicles: vehicles, drivers: drivers, sink: sink}
}

// Plan pulls confirmed orders for the date, optimizes the stop sequence, and
// persists a DRAFT plan for dispatcher fine-tuning.
func (s *Service) Plan(ctx context.Context, req PlanRequest) (*Plan, error) {
	if req.Date == "" {
		return nil, fmt.Errorf("date is required")
	}

	orders, err := s.orders.ListOrdersForDate(ctx, req.Date)
	if err != nil {
		return nil, fmt.Errorf("fetch orders: %w", err)
	}

	// Build stops from orders that carry geolocation.
	var stops []Stop
	var sumLat, sumLng float64
	for _, o := range orders {
		if o.Latitude == nil || o.Longitude == nil {
			continue
		}
		var weight float64
		for _, l := range o.Lines {
			weight += l.WeightLbs * l.Quantity
		}
		stops = append(stops, Stop{
			OrderID:   o.ID,
			Lat:       *o.Latitude,
			Lng:       *o.Longitude,
			Address:   o.Address,
			WeightLbs: weight,
		})
		sumLat += *o.Latitude
		sumLng += *o.Longitude
	}

	// Depot defaults to the centroid of all stops when not supplied.
	depotLat, depotLng := 0.0, 0.0
	if req.DepotLat != nil && req.DepotLng != nil {
		depotLat, depotLng = *req.DepotLat, *req.DepotLng
	} else if len(stops) > 0 {
		depotLat = sumLat / float64(len(stops))
		depotLng = sumLng / float64(len(stops))
	}

	// Pull the live fleet and bin-pack stops across trucks by capacity (CVRP).
	vehicles, err := s.vehicles.ListVehicles(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch vehicles: %w", err)
	}
	loads, unassigned := assignLoads(vehicles, stops)

	// GableLBM requires a valid driver id on write-back. Assign drivers to loads
	// round-robin (deterministic, ACTIVE first) so each load can be pushed.
	drivers, err := s.drivers.ListDrivers(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch drivers: %w", err)
	}
	assignDrivers(drivers, loads)

	// Sequence each load independently and aggregate the plan-level totals.
	flattened := []Stop{}
	var totalDistance, totalDuration float64
	for i := range loads {
		ordered, distance, duration := active.Sequence(ctx, depotLat, depotLng, loads[i].Stops)
		if ordered == nil {
			ordered = []Stop{}
		}
		loads[i].Stops = ordered
		loads[i].TotalDistanceMi = distance
		loads[i].TotalDurationMin = duration
		flattened = append(flattened, ordered...)
		totalDistance += distance
		totalDuration += duration
	}
	if unassigned == nil {
		unassigned = []Stop{}
	}

	plan := &Plan{
		PlanDate:         req.Date,
		GableBranchID:    req.BranchID,
		GableVehicleID:   req.VehicleID,
		Loads:            loads,
		UnassignedStops:  unassigned,
		Stops:            flattened,
		TotalDistanceMi:  round2(totalDistance),
		TotalDurationMin: round2(totalDuration),
		Status:           "DRAFT",
	}
	if err := s.repo.Save(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// Get returns a stored plan by id.
func (s *Service) Get(ctx context.Context, id string) (*Plan, error) {
	return s.repo.Get(ctx, id)
}

// Approve marks a plan APPROVED and writes the route back to GableLBM.
func (s *Service) Approve(ctx context.Context, id string) (*Plan, error) {
	plan, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if len(plan.Loads) == 0 {
		return nil, fmt.Errorf("plan has no loads assigned; cannot write back")
	}

	// One delivery_route per load. Each push is idempotent upstream on
	// (vehicle_id, scheduled_date).
	for _, load := range plan.Loads {
		route := gable.DeliveryRoute{
			VehicleID:     load.VehicleID,
			DriverID:      load.DriverID,
			ScheduledDate: plan.PlanDate,
		}
		for _, st := range load.Stops {
			route.Stops = append(route.Stops, gable.RouteStop{
				OrderID:  st.OrderID,
				Sequence: st.Sequence,
				Lat:      st.Lat,
				Lng:      st.Lng,
			})
		}
		if err := s.sink.PushDeliveryRoute(ctx, route); err != nil {
			return nil, fmt.Errorf("write back to GableLBM (vehicle %s): %w", load.VehicleID, err)
		}
	}

	if err := s.repo.UpdateStatus(ctx, id, "APPROVED"); err != nil {
		return nil, err
	}
	plan.Status = "APPROVED"
	return plan, nil
}
