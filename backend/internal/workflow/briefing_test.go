// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"strings"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/load"
)

// TestBuildBriefingPromptIncludesRisks verifies the deterministic prompt embeds
// the real callouts the briefing must reference: priority orders, long loads,
// GVW/axle flags, and compliance reroutes — and nothing the model could invent.
func TestBuildBriefingPromptIncludesRisks(t *testing.T) {
	p := &Plan{
		PlanDate: "2026-06-26",
		Status:   StatusReviewed,
		Orders: []OrderAnalysis{
			{OrderID: "o1", CustomerName: "Acme Framing", ShapeProfile: ShapeLongLoad, MaxLengthIn: 240, Priority: true, Routable: true},
			{OrderID: "o2", CustomerName: "Bob Builders", ShapeProfile: ShapeCompact},
		},
		Loads: []TruckLoad{
			{
				VehicleName: "Flatbed 1", DriverName: "Dana", CapacityWeightLbs: 20000,
				TotalWeightLbs: 18500, TotalDistanceMi: 42.5, TotalDurationMin: 75,
				Stops: []Stop{
					{OrderID: "o1", Sequence: 1, CustomerName: "Acme Framing", WeightLbs: 12000, Priority: true},
					{OrderID: "o2", Sequence: 2, CustomerName: "Bob Builders", WeightLbs: 6500},
				},
				LoadPlan: &load.Plan{
					TotalWeightLbs: 18500,
					GVWStatus:      "WARN",
					AxleLoads: []load.AxleLoad{
						{AxleNumber: 2, WeightLbs: 21500, MaxWeightLbs: 21000, Status: "FAIL"},
					},
				},
				Compliance: &ComplianceReview{
					Status:  "WARN",
					Actions: []ComplianceAction{{Type: "REROUTE", Description: "Detour around Mill St bridge", Resolved: true}},
				},
			},
		},
	}

	out := buildBriefingPrompt(p)

	for _, want := range []string{
		"2026-06-26",
		"Acme Framing",
		"PRIORITY",
		"LONG/OVERSIZE",
		"GVW WARN",
		"Axle 2 FAIL",
		"REROUTE",
		"Flatbed 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("briefing prompt missing %q\n---\n%s", want, out)
		}
	}
}
