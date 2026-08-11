// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package fleet manages per-vehicle supplementary profiles (axle layout, bed
// dimensions, GVWR, tare) keyed by GableLBM vehicle id. GableLBM owns the
// vehicle records; AI_LM only stores the load-planning attributes the ERP lacks.
package fleet

import "time"

// Axle describes a single axle's rated limit and position along the chassis.
type Axle struct {
	ID                  string  `json:"id"`
	AxleNumber          int     `json:"axle_number"`
	MaxWeightLbs        int64   `json:"max_weight_lbs"`
	PositionFromFrontIn float64 `json:"position_from_front_in"`
	AxleType            string  `json:"axle_type"` // STEER/DRIVE/TRAILER/TAG
}

// Profile is the supplementary load-planning data for one GableLBM vehicle.
type Profile struct {
	ID             string    `json:"id"`
	GableVehicleID string    `json:"gable_vehicle_id"`
	Name           string    `json:"name"`
	BedLengthIn    float64   `json:"bed_length_in"`
	BedWidthIn     float64   `json:"bed_width_in"`
	BedHeightIn    float64   `json:"bed_height_in"`
	GVWRLbs        int64     `json:"gvwr_lbs"`
	TareWeightLbs  int64     `json:"tare_weight_lbs"`
	Axles          []Axle    `json:"axles"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ProfileInput is the upsert payload for a vehicle profile.
type ProfileInput struct {
	Name          string      `json:"name"`
	BedLengthIn   float64     `json:"bed_length_in"`
	BedWidthIn    float64     `json:"bed_width_in"`
	BedHeightIn   float64     `json:"bed_height_in"`
	GVWRLbs       int64       `json:"gvwr_lbs"`
	TareWeightLbs int64       `json:"tare_weight_lbs"`
	Axles         []AxleInput `json:"axles"`
}

// AxleInput is a single axle in an upsert payload.
type AxleInput struct {
	AxleNumber          int     `json:"axle_number"`
	MaxWeightLbs        int64   `json:"max_weight_lbs"`
	PositionFromFrontIn float64 `json:"position_from_front_in"`
	AxleType            string  `json:"axle_type"`
}
