// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package fleet

import "context"

// Service holds fleet-profile business logic.
type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

// ListProfiles returns all vehicle profiles.
func (s *Service) ListProfiles(ctx context.Context) ([]Profile, error) {
	return s.repo.List(ctx)
}

// GetProfile returns a single profile by GableLBM vehicle id.
func (s *Service) GetProfile(ctx context.Context, gableVehicleID string) (*Profile, error) {
	return s.repo.GetByVehicleID(ctx, gableVehicleID)
}

// UpsertProfile creates or replaces a vehicle profile.
func (s *Service) UpsertProfile(ctx context.Context, gableVehicleID string, in ProfileInput) (*Profile, error) {
	return s.repo.Upsert(ctx, gableVehicleID, in)
}
