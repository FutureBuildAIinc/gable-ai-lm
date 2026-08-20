// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * Load-safety readouts on the Pack step.
 *
 * `PlanWorkflow.test.ts` covers the happy-path numbers a dispatcher reads.
 * This file covers the *verdict* rendering — the colour and wording that tell
 * someone whether a truck is safe — with the fixture deliberately trimmed to
 * one truck so each case states exactly which field it is exercising.
 *
 * The load in question here is the one the solver could NOT judge:
 * `internal/load/model.go` emits `status: "UNKNOWN"` for an axle whose fleet
 * profile carries no rating, sets `utilization` to 0 for want of a denominator,
 * and flags the plan `profile_status: "INCOMPLETE"`. Its own docblock is
 * explicit that "StatusUnknown is NOT a degraded PASS" and that the UI "must say
 * so rather than showing a green badge".
 *
 * The 3D visualiser and the Leaflet map are stubbed: they need WebGL and a real
 * layout box, and compute none of the numbers under test.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../components/load/Load3DVisualizer.ts', () => ({}));
vi.mock('../components/routing/RouteMap.ts', () => ({}));

import type { PlanWorkflow } from './PlanWorkflow.ts';
import './PlanWorkflow.ts';
import type { AxleLoad, LoadPlan, WorkflowPlan } from '../services/aiLmService.ts';
import { clickByText, jsonResponse, mount, text } from '../test/dom.ts';

const grouped = (n: number) => n.toLocaleString();

/** A fully rated two-axle plan — the control every UNKNOWN case is compared to. */
function ratedPlan(): LoadPlan {
  return {
    id: 'lp-1',
    gable_vehicle_id: 'veh-1',
    total_weight_lbs: 28_400,
    balance_score: 0.912,
    gvw_status: 'PASS',
    unplaced: [],
    max_load_height_in: 44.2,
    profile_status: 'COMPLETE',
    profile_issues: [],
    placements: [],
    created_at: '2026-08-11T15:00:00Z',
    axle_loads: [
      {
        axle_number: 1,
        weight_lbs: 9_600,
        max_weight_lbs: 12_000,
        utilization: 0.8,
        status: 'PASS',
        advisory: true,
      },
      {
        axle_number: 2,
        weight_lbs: 18_800,
        max_weight_lbs: 21_000,
        utilization: 0.895,
        status: 'PASS',
        advisory: true,
      },
    ],
  };
}

/** One truck, packed, on a plan already at REVIEWED so the Pack step is reachable. */
function planWith(loadPlan: LoadPlan): WorkflowPlan {
  return {
    id: 'plan-1',
    plan_date: '2026-08-12',
    status: 'REVIEWED',
    depot_lat: 49.8863,
    depot_lng: -119.4666,
    orders: [],
    unassigned_orders: [],
    created_at: '2026-08-11T14:00:00Z',
    updated_at: '2026-08-11T15:12:00Z',
    loads: [
      {
        vehicle_id: 'veh-1',
        vehicle_name: 'Freightliner M2 Flatbed',
        driver_name: 'Marc T.',
        capacity_weight_lbs: 19_000,
        total_weight_lbs: 12_345,
        total_distance_mi: 18.4,
        total_duration_min: 47,
        bed: { length_in: 288, width_in: 96, height_in: 96 },
        stops: [
          {
            order_id: 'o-1',
            sequence: 1,
            lat: 49.87,
            lng: -119.45,
            address: '2200 Benvoulin Rd',
            customer_name: 'Ridgeview Framing',
            weight_lbs: 12_345,
            priority: false,
          },
        ],
        load_plan: loadPlan,
        compliance: {
          status: 'PASS',
          flags: [],
          actions: [],
          checked_gross_lbs: loadPlan.total_weight_lbs,
          checked_max_axle_lbs: 18_800,
          checked_height_in: 44.2,
        },
        proof: {
          attachments: [{ url: 'https://cdn.example.test/a.jpg', kind: 'PHOTO', added_at: 'x' }],
          signed_off: true,
        },
      },
    ],
  };
}

/** Mount at the Pack step with `mutate` applied to a fully rated load plan. */
async function packStep(mutate: (lp: LoadPlan) => void = () => {}): Promise<PlanWorkflow> {
  const lp = ratedPlan();
  mutate(lp);
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(planWith(lp))));
  const el = await mount<PlanWorkflow>('ailm-plan-workflow');
  await clickByText(el, 'button', 'Pack Loads');
  return el;
}

/** The "Axle Loads" panel. */
function axleCard(el: PlanWorkflow): Element {
  const card = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
    text(c).startsWith('Axle Loads'),
  );
  if (!card) throw new Error('no Axle Loads card rendered');
  return card;
}

/** The filled portion of each axle-utilisation bar, in render order. */
function axleBars(el: PlanWorkflow): HTMLElement[] {
  return Array.from(axleCard(el).querySelectorAll('.h-full')) as HTMLElement[];
}

function barWidth(bar: HTMLElement): number {
  const match = /width:\s*([\d.]+)%/.exec(bar.getAttribute('style') ?? '');
  if (!match) throw new Error(`no width in style: ${bar.getAttribute('style')}`);
  return Number(match[1]);
}

/** An axle the fleet profile never rated, exactly as the solver emits it. */
function unratedAxle(axleNumber: number, weightLbs: number): AxleLoad {
  return {
    axle_number: axleNumber,
    weight_lbs: weightLbs,
    max_weight_lbs: 0, // blank rating on the fleet profile
    utilization: 0, // no denominator — NOT "this axle is empty"
    status: 'UNKNOWN',
    advisory: true,
  };
}

beforeEach(() => {
  window.history.replaceState({}, '', '/plan');
});

describe('PlanWorkflow — an axle the solver could not judge', () => {
  it('does not paint an UNKNOWN axle with the PASS colour', async () => {
    const el = await packStep((lp) => {
      lp.axle_loads[1] = unratedAxle(2, 18_800);
      lp.gvw_status = 'FAIL'; // UNKNOWN collapses to FAIL (load/solver.go overallStatus)
      lp.profile_status = 'INCOMPLETE';
      lp.profile_issues = ['axle 2 has no rated capacity — its load cannot be judged'];
    });

    const [rated, unrated] = axleBars(el);
    expect(rated.className).toContain('bg-gable-green'); // control: a real PASS
    expect(unrated.className).not.toContain('bg-gable-green');
  });

  it('does not draw an UNKNOWN axle as an empty bar', async () => {
    // utilization is 0 because the axle has no rating, so a proportional bar
    // renders 0% wide — visually identical to "nothing is on this axle".
    const el = await packStep((lp) => {
      lp.axle_loads[1] = unratedAxle(2, 18_800);
    });

    expect(barWidth(axleBars(el)[1])).toBeGreaterThan(0);
  });

  it('says the axle is unrated instead of printing a 0 lb rating', async () => {
    const el = await packStep((lp) => {
      lp.axle_loads[1] = unratedAxle(2, 18_800);
    });

    const body = text(axleCard(el));
    expect(body).toContain(`Axle 2 ${grouped(18_800)} lb / UNRATED`);
    expect(body).toMatch(/cannot be judged/i);
    // The old rendering read "18,800 / 0 lb", which parses as a rating of zero
    // rather than as a missing rating.
    expect(body).not.toContain(`${grouped(18_800)} / 0 lb`);
  });

  it('still reports the real weight carried on the unrated axle', async () => {
    // Whatever the UI cannot certify, it must not hide: the pounds are known.
    const el = await packStep((lp) => {
      lp.axle_loads[1] = unratedAxle(2, 18_800);
    });

    expect(text(axleCard(el))).toContain(grouped(18_800));
  });

  it('leaves the rated axles in the same plan judged normally', async () => {
    const el = await packStep((lp) => {
      lp.axle_loads[0].status = 'WARN';
      lp.axle_loads[0].utilization = 0.96;
      lp.axle_loads[1] = unratedAxle(2, 18_800);
    });

    const bars = axleBars(el);
    expect(bars[0].className).toContain('bg-amber-warn');
    expect(barWidth(bars[0])).toBeCloseTo(96, 6);
    expect(text(axleCard(el))).toContain(`Axle 1 ${grouped(9_600)} / ${grouped(12_000)} lb`);
  });

  it('keeps colouring FAIL and WARN axles by severity', async () => {
    const el = await packStep((lp) => {
      lp.axle_loads[0].status = 'WARN';
      lp.axle_loads[1].status = 'FAIL';
      lp.axle_loads[1].utilization = 1.12;
    });

    const bars = axleBars(el);
    expect(bars[0].className).toContain('bg-amber-warn');
    expect(bars[1].className).toContain('bg-safety-red');
    expect(barWidth(bars[1])).toBeCloseTo(112, 6);
  });
});

describe('PlanWorkflow — fleet-profile completeness', () => {
  it('warns that an INCOMPLETE profile makes the verdicts untrustworthy', async () => {
    const el = await packStep((lp) => {
      lp.profile_status = 'INCOMPLETE';
      lp.profile_issues = [
        'axle 2 has no rated capacity — its load cannot be judged',
        'fleet profile has no GVWR — gross weight cannot be judged',
      ];
      lp.axle_loads[1] = unratedAxle(2, 18_800);
      lp.gvw_status = 'FAIL';
    });

    const body = text(axleCard(el));
    expect(body).toContain('Fleet profile incomplete');
    expect(body).toContain('not trustworthy');
  });

  it('names every specific defect the solver reported', async () => {
    const el = await packStep((lp) => {
      lp.profile_status = 'INCOMPLETE';
      lp.profile_issues = [
        'axle 2 has no rated capacity — its load cannot be judged',
        'fleet profile has no GVWR — gross weight cannot be judged',
      ];
    });

    const items = Array.from(axleCard(el).querySelectorAll('li')).map((li) => text(li));
    expect(items).toEqual([
      'axle 2 has no rated capacity — its load cannot be judged',
      'fleet profile has no GVWR — gross weight cannot be judged',
    ]);
  });

  it('stays quiet on a COMPLETE profile', async () => {
    const el = await packStep();
    expect(text(axleCard(el))).not.toContain('Fleet profile incomplete');
  });

  it('stays quiet when the backend omits the field entirely', async () => {
    // profile_status/profile_issues are `omitempty` in Go, so an older AI_LM
    // backend sends neither. Absence must not be read as "incomplete".
    const el = await packStep((lp) => {
      delete lp.profile_status;
      delete lp.profile_issues;
    });

    expect(text(axleCard(el))).not.toContain('Fleet profile incomplete');
    expect(text(axleCard(el))).toContain('Axle 1');
  });

  // The balance score must never be rendered as a flat percentage without
  // reference to profile completeness. When the profile is INCOMPLETE the solver
  // deliberately leaves BalanceScore at 0 ("not computable — left at 0 alongside
  // ProfileIncomplete", backend/internal/load/solver.go), and a panel reading
  // "Balance score 0%" tells a dispatcher "catastrophically unbalanced" when the
  // truth is "not measured". It renders "— not computed" instead, the same way
  // an unrated axle does.
  it('does not report a computed balance score for an unjudgeable profile', async () => {
    const el = await packStep((lp) => {
      lp.profile_status = 'INCOMPLETE';
      lp.profile_issues = ['fleet profile has no axles — per-axle load cannot be computed'];
      lp.balance_score = 0;
    });

    expect(text(axleCard(el))).not.toContain('Balance score 0%');
  });
});

describe('PlanWorkflow — GVW banner', () => {
  it('reports a compliant gross weight', async () => {
    const el = await packStep();
    expect(text(el)).toContain('GVW PASS — within all axle and gross limits');
    expect(text(el)).toContain(`${grouped(28_400)} lb gross`);
  });

  it('reports a load approaching a rated limit', async () => {
    const el = await packStep((lp) => {
      lp.gvw_status = 'WARN';
    });
    expect(text(el)).toContain('GVW WARNING — approaching a rated limit');
  });

  it('reports an overweight load', async () => {
    const el = await packStep((lp) => {
      lp.gvw_status = 'FAIL';
    });
    expect(text(el)).toContain('GVW FAIL — overweight; redistribute or remove load');
  });

  it('rounds the load height to whole inches', async () => {
    const el = await packStep((lp) => {
      lp.max_load_height_in = 137.6;
    });
    expect(text(el)).toContain('load 138″ tall');
  });

  it('renders a plan with no measured height as 0″ rather than NaN', async () => {
    const el = await packStep((lp) => {
      delete lp.max_load_height_in;
    });
    expect(text(el)).toContain('load 0″ tall');
    expect(text(el)).not.toContain('NaN');
  });
});

describe('PlanWorkflow — bed volume budget', () => {
  it('reports cargo against usable volume with a utilisation percentage', async () => {
    const el = await packStep((lp) => {
      lp.bed_volume_cuft = 942.1;
      lp.usable_volume_cuft = 612.4;
      lp.cargo_volume_cuft = 431.8;
      lp.volume_utilization = 0.71;
      lp.volume_status = 'WARN';
    });

    expect(text(axleCard(el))).toContain('Bed volume 432 / 612 ft³ (71%)');
  });

  it('omits the volume row when the backend sent no volume budget', async () => {
    const el = await packStep();
    expect(text(axleCard(el))).not.toContain('Bed volume');
  });
});

describe('PlanWorkflow — securement', () => {
  const securement = {
    cargo_weight_lbs: 20_210,
    min_aggregate_wll_lbs: 10_105,
    recommended_strap: '4" winch strap (WLL 5400 lb)',
    jurisdiction: 'US_FMCSA',
    ruleset_name: '49 CFR 393.100–136',
    rule_basis: 'FMCSA cargo securement',
    required_tie_downs: 4,
    straps: [
      { number: 1, position_in: 18, over_height_in: 40, required_wll_lbs: 2600 },
      { number: 2, position_in: 150, over_height_in: 40, required_wll_lbs: 2600 },
    ],
    notes: ['Use edge protectors wherever a strap crosses a board edge.'],
  };

  function secCard(el: PlanWorkflow): Element {
    const card = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
      text(c).startsWith('Securement'),
    );
    if (!card) throw new Error('no Securement card rendered');
    return card;
  }

  it('states the aggregate WLL as half the cargo weight, per 49 CFR 393', async () => {
    const el = await packStep((lp) => {
      lp.securement = securement;
    });

    expect(text(secCard(el))).toContain(
      `Required aggregate WLL ${grouped(10_105)} lb (50% of ${grouped(20_210)} lb)`,
    );
  });

  it('converts each strap position from inches to feet from the nose', async () => {
    const el = await packStep((lp) => {
      lp.securement = securement;
    });

    const body = text(secCard(el));
    expect(body).toContain('1.5 ft from nose'); // 18 in
    expect(body).toContain('12.5 ft from nose'); // 150 in
  });

  it('cites the jurisdiction and its rule basis', async () => {
    const el = await packStep((lp) => {
      lp.securement = securement;
    });

    const badge = secCard(el).querySelector('[title="FMCSA cargo securement"]');
    expect(badge?.textContent?.trim()).toBe('US_FMCSA');
    expect(text(secCard(el))).toContain('Ruleset: 49 CFR 393.100–136');
  });

  it('reports the rule minimum alongside the tie-downs it planned', async () => {
    // The planner laid out 2 straps but the ruleset demands 4 — the shortfall is
    // the whole point of printing both numbers.
    const el = await packStep((lp) => {
      lp.securement = securement;
    });

    const body = text(secCard(el));
    expect(body).toContain('2 tie-down(s)');
    expect(body).toContain('rule min 4');
  });

  it('omits the card entirely when the backend computed no securement', async () => {
    const el = await packStep();
    expect(
      Array.from(el.querySelectorAll('.glass-card')).some((c) =>
        text(c).startsWith('Securement'),
      ),
    ).toBe(false);
  });
});

describe('PlanWorkflow — truck tab status dot', () => {
  /** The colour of the status dot on the truck tab button. */
  function dotColour(el: PlanWorkflow): string {
    const dot = el.querySelector('button .h-2.w-2');
    return /background:\s*([^;"]+)/.exec(dot?.getAttribute('style') ?? '')?.[1] ?? '';
  }

  it('shows compliance FAIL in the safety red', async () => {
    const lp = ratedPlan();
    const plan = planWith(lp);
    plan.loads[0].compliance!.status = 'FAIL';
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(plan)));
    const el = await mount<PlanWorkflow>('ailm-plan-workflow');
    await clickByText(el, 'button', 'Pack Loads');

    expect(dotColour(el)).toBe('#F43F5E');
  });

  it('falls back to the load plan verdict when the route was not checked', async () => {
    const lp = ratedPlan();
    lp.gvw_status = 'WARN';
    const plan = planWith(lp);
    delete plan.loads[0].compliance;
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(plan)));
    const el = await mount<PlanWorkflow>('ailm-plan-workflow');
    await clickByText(el, 'button', 'Pack Loads');

    expect(dotColour(el)).toBe('#FBBF24');
  });

  it('greys the dot when nothing has judged the truck yet', async () => {
    const plan = planWith(ratedPlan());
    delete plan.loads[0].compliance;
    delete plan.loads[0].load_plan;
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(plan)));
    const el = await mount<PlanWorkflow>('ailm-plan-workflow');
    await clickByText(el, 'button', 'Pack Loads');

    expect(dotColour(el)).toBe('#71717a');
  });
});

describe('PlanWorkflow — unplaced cargo', () => {
  it('names every SKU the packer could not fit', async () => {
    const el = await packStep((lp) => {
      lp.unplaced = ['WRC-2X8-20', 'LVL-1.75X11.875-24'];
    });

    expect(text(el)).toContain('Did not fit: WRC-2X8-20, LVL-1.75X11.875-24');
  });

  it('says nothing about unplaced cargo when everything fit', async () => {
    const el = await packStep();
    expect(text(el)).not.toContain('Did not fit');
  });
});
