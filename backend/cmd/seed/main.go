// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Command seed writes ONE FICTIONAL DEALER's fleet profiles and restricted
// points into an AI_LM database. It exists so a demo or a fresh development
// checkout has something to plan against; it is not part of installing AI_LM.
//
// # Opt-in
//
// The command does nothing unless DEMO_SEED is set to an affirmative value
// ("1", "true", "yes" or "on"). Absence is the safe default, so a production
// or self-hosted install that runs the deploy pipeline unchanged never
// acquires this data. The DigitalOcean POST_DEPLOY job applies the same gate
// in the shell (`if [ "$DEMO_SEED" = "true" ]; then ./seed; fi`) so the seed
// binary usually is not even invoked — the check here is the backstop for
// `make seed`, docker-compose, and anyone running the binary by hand.
//
// # Idempotent
//
// Re-running converges rather than duplicating. Fleet profiles upsert on
// gable_vehicle_id; restricted points upsert on their name as a natural key,
// updating a drifted row in place and leaving an already-correct one alone.
// A deploy pipeline may therefore run it on every release without the
// compliance registry growing three rows at a time.
//
// The vehicle ids below are fabricated UUIDs. In a real deployment a profile's
// gable_vehicle_id matches a vehicle in the dealer's own GableLBM fleet.
package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/compliance"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/config"
	"github.com/FutureBuildAIinc/gable-ai-lm/internal/fleet"
	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/database"
)

// demoSeedEnv gates the entire command. Keep this name in sync with the
// POST_DEPLOY job in .do/*.yaml — the manifest gate and this gate are two
// halves of one contract.
const demoSeedEnv = "DEMO_SEED"

// Fabricated vehicle ids for the demo dealer. They are namespaced into an
// obviously-synthetic UUID range so a real fleet id can never collide with one.
const (
	demoFlatbedVehicleID  = "11111111-1111-1111-1111-111111111111"
	demoBoxTruckVehicleID = "22222222-2222-2222-2222-222222222222"
)

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

func main() {
	// Checked before anything else: when the seed is not requested the command
	// must not need a database, a config, or any credentials at all.
	if !demoSeedRequested() {
		log.Printf(
			"%s is not set — skipping the demo seed. No fleet profiles or restricted points were written. "+
				"This is the correct outcome for a production or self-hosted install: AI_LM ships with an empty "+
				"registry and expects the dealer's own fleet and restricted points. Set %s=1 only on a demo or "+
				"development environment.",
			demoSeedEnv, demoSeedEnv,
		)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	db, err := database.Connect(cfg.DatabaseURL, database.DefaultPoolConfig())
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("%s is set — seeding one fictional dealer's demo data (idempotent; re-runs converge)", demoSeedEnv)

	if err := seedFleetProfiles(ctx, fleet.NewService(fleet.NewRepository(db))); err != nil {
		log.Fatalf("seed fleet profiles: %v", err)
	}
	if err := seedRestrictedPoints(ctx, compliance.NewService(compliance.NewRepository(db))); err != nil {
		log.Fatalf("seed restricted points: %v", err)
	}

	log.Println("seed complete")
}

// demoSeedRequested reports whether the operator explicitly asked for demo
// data. Anything other than an affirmative value — including unset, empty,
// "0" and "false" — means no.
func demoSeedRequested() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(demoSeedEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// --- fleet profiles ----------------------------------------------------------

// demoProfiles is the fictional dealer's two trucks.
func demoProfiles() []struct {
	vehicleID string
	input     fleet.ProfileInput
} {
	return []struct {
		vehicleID string
		input     fleet.ProfileInput
	}{
		{
			vehicleID: demoFlatbedVehicleID,
			input: fleet.ProfileInput{
				Name:          "Freightliner M2 Flatbed",
				BedLengthIn:   288, // 24 ft
				BedWidthIn:    96,  // 8 ft
				BedHeightIn:   96,
				GVWRLbs:       33000,
				TareWeightLbs: 14000,
				Axles: []fleet.AxleInput{
					{AxleNumber: 1, MaxWeightLbs: 12000, PositionFromFrontIn: 0, AxleType: "STEER"},
					{AxleNumber: 2, MaxWeightLbs: 21000, PositionFromFrontIn: 240, AxleType: "DRIVE"},
				},
			},
		},
		{
			vehicleID: demoBoxTruckVehicleID,
			input: fleet.ProfileInput{
				Name:          "International Box Truck",
				BedLengthIn:   312, // 26 ft
				BedWidthIn:    100,
				BedHeightIn:   102,
				GVWRLbs:       26000,
				TareWeightLbs: 12500,
				Axles: []fleet.AxleInput{
					{AxleNumber: 1, MaxWeightLbs: 10000, PositionFromFrontIn: 0, AxleType: "STEER"},
					{AxleNumber: 2, MaxWeightLbs: 17500, PositionFromFrontIn: 260, AxleType: "DRIVE"},
				},
			},
		},
	}
}

// seedFleetProfiles upserts on gable_vehicle_id (fleet.Repository.Upsert is
// ON CONFLICT ... DO UPDATE and replaces the axle set inside one transaction),
// so a repeat run rewrites the same two rows.
func seedFleetProfiles(ctx context.Context, svc *fleet.Service) error {
	for _, p := range demoProfiles() {
		if _, err := svc.UpsertProfile(ctx, p.vehicleID, p.input); err != nil {
			return fmt.Errorf("vehicle profile %q: %w", p.input.Name, err)
		}
		log.Printf("fleet profile upserted: %s (%s)", p.input.Name, p.vehicleID)
	}
	return nil
}

// --- restricted points -------------------------------------------------------

// demoPoints is the fictional dealer's Okanagan-corridor restriction registry.
// The limits are demo-calibrated so a loaded truck actually trips them; they
// are not a survey of real infrastructure.
func demoPoints() []compliance.RestrictedPointInput {
	return []compliance.RestrictedPointInput{
		{
			Name:              "Bennett Bridge (W.R. Bennett)",
			Lat:               49.8845,
			Lng:               -119.4960,
			RestrictionType:   "WEIGHT",
			MaxGrossWeightLbs: i64(21000),
			Notes:             "Floating bridge — temporary gross-weight restriction during deck repair.",
		},
		{
			Name:            "Highway 97 CN Overpass",
			Lat:             49.8612,
			Lng:             -119.4490,
			RestrictionType: "HEIGHT",
			MaxHeightIn:     f64(136), // 11'4"
			Notes:           "Low clearance overpass.",
		},
		{
			Name:             "McCulloch Rd Culvert",
			Lat:              49.8420,
			Lng:              -119.3700,
			RestrictionType:  "WEIGHT",
			MaxAxleWeightLbs: i64(18000),
			Notes:            "Seasonal axle-weight limit on rural culvert.",
		},
	}
}

// seedRestrictedPoints upserts each demo point on its name.
//
// compliance.Repository.Create is a bare INSERT with no uniqueness constraint
// behind it, so calling it unconditionally is what made the old seed add three
// more rows on every deploy. Matching on the natural key first turns the same
// service calls into a converging upsert without a schema change.
func seedRestrictedPoints(ctx context.Context, svc *compliance.Service) error {
	existing, err := svc.ListPoints(ctx)
	if err != nil {
		return fmt.Errorf("list existing points: %w", err)
	}

	byKey := make(map[string]compliance.RestrictedPoint, len(existing))
	for _, p := range existing {
		byKey[naturalKey(p.Name)] = p
	}

	wanted := demoPoints()
	for _, want := range wanted {
		cur, found := byKey[naturalKey(want.Name)]
		switch {
		case !found:
			if _, err := svc.CreatePoint(ctx, want); err != nil {
				return fmt.Errorf("create %q: %w", want.Name, err)
			}
			log.Printf("restricted point created: %s", want.Name)
		case pointMatches(cur, want):
			log.Printf("restricted point already current, left alone: %s", want.Name)
		default:
			if _, err := svc.UpdatePoint(ctx, cur.ID, want); err != nil {
				return fmt.Errorf("update %q: %w", want.Name, err)
			}
			log.Printf("restricted point updated in place: %s", want.Name)
		}
	}

	// A registry that already holds points this seed does not own means the
	// database is not a scratch demo. Say so loudly; the operator may have
	// pointed DEMO_SEED at a real dealer's database.
	if foreign := countForeign(existing, wanted); foreign > 0 {
		log.Printf(
			"WARNING: %d restricted point(s) in this database were not written by the demo seed. "+
				"They are untouched, but a real registry suggests %s should not be set here.",
			foreign, demoSeedEnv,
		)
	}
	return nil
}

// naturalKey normalises a point name for matching: case- and
// whitespace-insensitive, so "bennett  bridge" finds "Bennett Bridge".
func naturalKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// pointMatches reports whether a stored point already carries every value the
// seed would write, so an unchanged row is left with its original timestamps.
func pointMatches(cur compliance.RestrictedPoint, want compliance.RestrictedPointInput) bool {
	return sameCoord(cur.Lat, want.Lat) &&
		sameCoord(cur.Lng, want.Lng) &&
		strings.EqualFold(cur.RestrictionType, want.RestrictionType) &&
		sameI64(cur.MaxGrossWeightLbs, want.MaxGrossWeightLbs) &&
		sameI64(cur.MaxAxleWeightLbs, want.MaxAxleWeightLbs) &&
		sameF64(cur.MaxHeightIn, want.MaxHeightIn) &&
		cur.Notes == want.Notes
}

func countForeign(existing []compliance.RestrictedPoint, wanted []compliance.RestrictedPointInput) int {
	owned := make(map[string]struct{}, len(wanted))
	for _, w := range wanted {
		owned[naturalKey(w.Name)] = struct{}{}
	}
	n := 0
	for _, p := range existing {
		if _, ok := owned[naturalKey(p.Name)]; !ok {
			n++
		}
	}
	return n
}

// sameCoord compares latitudes/longitudes with a tolerance well below the
// ~0.1 mm that the seventh decimal place represents, so a float round-trip
// through Postgres does not read as drift.
func sameCoord(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func sameI64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameF64(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) < 1e-9
}
