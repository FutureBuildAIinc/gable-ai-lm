// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

// Proof-of-load + sign-off gate (T1-6).
//
// Before a truck leaves the yard it must carry visual proof of how it was
// loaded plus a staff sign-off. The push step (the truck "departing" to the
// GableLBM dispatch board) is blocked until every load is Ready().

import (
	"context"
	"strings"
	"time"
)

// loadByVehicle returns the plan's load for a vehicle id, or an error.
func loadByVehicle(p *Plan, vehicleID string) (*TruckLoad, error) {
	for i := range p.Loads {
		if p.Loads[i].VehicleID == vehicleID {
			return &p.Loads[i], nil
		}
	}
	return nil, refusedf("no load for vehicle %s in this plan", vehicleID)
}

// AttachProof records a yard photo/video reference on a packed load (T1-6).
// Adding new proof after a sign-off clears the sign-off so the change is
// re-confirmed.
func (s *Service) AttachProof(ctx context.Context, id, vehicleID string, req ProofRequest) (*Plan, error) {
	if strings.TrimSpace(req.URL) == "" {
		return nil, refusedf("url is required")
	}
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	l, err := loadByVehicle(p, vehicleID)
	if err != nil {
		return nil, err
	}
	if l.LoadPlan == nil {
		return nil, refusedf("truck %s is not packed yet — pack before attaching proof", l.VehicleName)
	}

	kind := strings.ToUpper(strings.TrimSpace(req.Kind))
	if kind != "VIDEO" {
		kind = "PHOTO"
	}
	if l.Proof == nil {
		l.Proof = &LoadProof{}
	}
	l.Proof.Attachments = append(l.Proof.Attachments, ProofAttachment{
		URL:     req.URL,
		Kind:    kind,
		Caption: req.Caption,
		AddedBy: req.AddedBy,
		AddedAt: time.Now(),
	})
	// New evidence supersedes a prior sign-off.
	if l.Proof.SignedOff {
		l.Proof.SignedOff = false
		l.Proof.SignedAt = nil
	}

	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// SignOffLoad records the yard sign-off that releases a load to depart (T1-6).
// Requires at least one proof attachment first.
func (s *Service) SignOffLoad(ctx context.Context, id, vehicleID string, req SignOffRequest) (*Plan, error) {
	if strings.TrimSpace(req.SignedBy) == "" {
		return nil, refusedf("signed_by is required")
	}
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	l, err := loadByVehicle(p, vehicleID)
	if err != nil {
		return nil, err
	}
	if l.Proof == nil || len(l.Proof.Attachments) == 0 {
		return nil, refusedf("truck %s needs at least one proof photo/video before sign-off", l.VehicleName)
	}

	now := time.Now()
	l.Proof.SignedOff = true
	l.Proof.SignedBy = req.SignedBy
	l.Proof.SignedRole = req.Role
	l.Proof.SignedAt = &now
	l.Proof.Note = req.Note

	if err := s.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}
