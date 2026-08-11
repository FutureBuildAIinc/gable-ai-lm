// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

// Pillar-6 LLM feature: a concise natural-language dispatch briefing for a
// plan. It summarizes the day's routes and surfaces risk callouts (overweight
// axles, restricted-point reroutes, long-load handling, priority deliveries)
// from the already-analyzed plan data. The prompt is built deterministically
// from the plan; the LLM only writes the prose. AI is optional — when no
// OpenRouter key is set the briefing is reported unavailable and nothing in the
// core workflow is blocked.

import (
	"context"
	"fmt"
	"strings"
)

const briefingSystemPrompt = "You are a dispatch supervisor for a lumber & building-materials yard. " +
	"Write a concise, professional dispatch briefing for the drivers and yard team. " +
	"Use short paragraphs or tight bullet points. Lead with the day's shape (trucks, stops, total weight), " +
	"then call out the real risks the data shows: overweight axles or gross-weight (GVW) flags, " +
	"restricted-point reroutes or compliance flags, long-load / oversize handling, and any priority " +
	"(deliver-first) stops. Be specific and reference truck and customer names. Do not invent data that " +
	"is not in the summary. Keep it under ~200 words."

// briefingMaxTokens caps the completion length.
const briefingMaxTokens = 700

// Briefing returns the LLM-generated dispatch briefing for a plan. When AI is
// not configured it returns an Available=false payload (HTTP 200) so the UI can
// show a clear "set OPENROUTER_API_KEY" hint without erroring.
func (s *Service) Briefing(ctx context.Context, id string) (*Briefing, error) {
	p, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if s.ai == nil || !s.ai.Configured() {
		return &Briefing{
			Available: false,
			Message:   "AI briefing unavailable — set OPENROUTER_API_KEY to enable LLM dispatch briefings.",
		}, nil
	}

	text, err := s.ai.Generate(ctx, briefingSystemPrompt, buildBriefingPrompt(p), briefingMaxTokens)
	if err != nil {
		// Degrade gracefully: surface the reason, never fail the request.
		return &Briefing{
			Available: false,
			Model:     s.ai.Model(),
			Message:   "AI briefing temporarily unavailable: " + err.Error(),
		}, nil
	}

	return &Briefing{
		Available: true,
		Model:     s.ai.Model(),
		Text:      text,
	}, nil
}

// buildBriefingPrompt renders the plan's analyzed data into a compact,
// LLM-friendly text summary. Only facts already computed by the workflow are
// included so the model has nothing to hallucinate.
func buildBriefingPrompt(p *Plan) string {
	var b strings.Builder

	fmt.Fprintf(&b, "DISPATCH DATE: %s\n", p.PlanDate)
	fmt.Fprintf(&b, "STAGE: %s\n", p.Status)

	totalWeight, totalDist := 0.0, 0.0
	totalStops := 0
	for _, l := range p.Loads {
		totalWeight += l.TotalWeightLbs
		totalDist += l.TotalDistanceMi
		totalStops += len(l.Stops)
	}
	fmt.Fprintf(&b, "FLEET: %d truck(s), %d stop(s), %.0f lb total cargo, %.1f route mi total\n",
		len(p.Loads), totalStops, totalWeight, totalDist)

	// Day-wide order shape callouts.
	var longLoads, priorityOrders []string
	for _, o := range p.Orders {
		if o.ShapeProfile == ShapeLongLoad {
			longLoads = append(longLoads, fmt.Sprintf("%s (%.0f ft)", orderLabel(o), o.MaxLengthIn/12))
		}
		if o.Priority {
			priorityOrders = append(priorityOrders, orderLabel(o))
		}
	}
	if len(longLoads) > 0 {
		fmt.Fprintf(&b, "LONG/OVERSIZE LOADS: %s\n", strings.Join(longLoads, "; "))
	}
	if len(priorityOrders) > 0 {
		fmt.Fprintf(&b, "PRIORITY (DELIVER-FIRST) ORDERS: %s\n", strings.Join(priorityOrders, "; "))
	}

	if len(p.UnassignedOrders) > 0 {
		names := make([]string, 0, len(p.UnassignedOrders))
		for _, st := range p.UnassignedOrders {
			names = append(names, stopLabel(st))
		}
		fmt.Fprintf(&b, "UNASSIGNED (no truck capacity): %s\n", strings.Join(names, "; "))
	}

	b.WriteString("\nTRUCKS:\n")
	for _, l := range p.Loads {
		driver := l.DriverName
		if driver == "" {
			driver = "unassigned driver"
		}
		fmt.Fprintf(&b, "- %s (%s): %d stop(s), %.0f / %d lb, %.1f mi, %.0f min\n",
			l.VehicleName, driver, len(l.Stops), l.TotalWeightLbs, l.CapacityWeightLbs,
			l.TotalDistanceMi, l.TotalDurationMin)

		// GVW / axle status from the packing plan.
		if l.LoadPlan != nil {
			if l.LoadPlan.GVWStatus != "" && l.LoadPlan.GVWStatus != "PASS" {
				fmt.Fprintf(&b, "    GVW %s — gross %d lb\n", l.LoadPlan.GVWStatus, l.LoadPlan.TotalWeightLbs)
			}
			for _, a := range l.LoadPlan.AxleLoads {
				if a.Status != "PASS" {
					fmt.Fprintf(&b, "    Axle %d %s: %d / %d lb\n", a.AxleNumber, a.Status, a.WeightLbs, a.MaxWeightLbs)
				}
			}
		}

		// Compliance review outcome + AI resolutions.
		if l.Compliance != nil {
			if l.Compliance.Status != "" && l.Compliance.Status != "PASS" {
				fmt.Fprintf(&b, "    Route compliance: %s (%d flag(s))\n", l.Compliance.Status, len(l.Compliance.Flags))
			}
			for _, act := range l.Compliance.Actions {
				state := "resolved"
				if !act.Resolved {
					state = "UNRESOLVED"
				}
				fmt.Fprintf(&b, "    %s [%s]: %s\n", act.Type, state, act.Description)
			}
			for _, f := range l.Compliance.Flags {
				fmt.Fprintf(&b, "    Flag %s: %s — %s\n", f.Severity, f.Point.Name, f.Violation)
			}
		}

		// Stop list (with priority markers).
		for _, st := range l.Stops {
			star := ""
			if st.Priority {
				star = " [PRIORITY]"
			}
			fmt.Fprintf(&b, "    %d. %s — %.0f lb%s\n", st.Sequence, stopLabel(st), st.WeightLbs, star)
		}
	}

	return b.String()
}

func orderLabel(o OrderAnalysis) string {
	if o.CustomerName != "" {
		return o.CustomerName
	}
	return "order " + o.OrderID
}
