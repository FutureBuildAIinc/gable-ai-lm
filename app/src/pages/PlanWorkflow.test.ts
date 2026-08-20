// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * <ailm-plan-workflow> renders the numbers a dispatcher signs off on: gross
 * weight, per-axle utilisation, load height, tie-down positions and the
 * PASS/WARN/FAIL badges derived from them. Every one of those is a unit
 * conversion or a rounding away from being wrong, and a wrong number here ends
 * up on a truck. These tests assert the rendered output, not the fixture.
 *
 * The 3D visualiser and the Leaflet map are stubbed out: they need WebGL and a
 * real layout box, neither of which jsdom has, and neither computes any of the
 * numbers under test.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../components/load/Load3DVisualizer.ts', () => ({}));
vi.mock('../components/routing/RouteMap.ts', () => ({}));

import type { PlanWorkflow } from './PlanWorkflow.ts';
import './PlanWorkflow.ts';
import type { WorkflowPlan } from '../services/aiLmService.ts';
import { clickByText, jsonResponse, mount, text } from '../test/dom.ts';

/** Locale-derived so the assertion survives a differently-configured runner. */
const grouped = (n: number) => n.toLocaleString();

/** Reads the percentage an axle-utilisation bar was rendered at. */
function barWidth(bar: HTMLElement): number {
  const match = /width:\s*([\d.]+)%/.exec(bar.getAttribute('style') ?? '');
  if (!match) throw new Error(`no width in style: ${bar.getAttribute('style')}`);
  return Number(match[1]);
}

function plan(): WorkflowPlan {
  return {
    id: 'plan-1',
    plan_date: '2026-08-12',
    status: 'REVIEWED',
    depot_lat: 49.8863,
    depot_lng: -119.4666,
    orders: [
      {
        order_id: 'o-1',
        customer_name: 'Ridgeview Framing',
        address: '2200 Benvoulin Rd',
        lat: 49.87,
        lng: -119.45,
        total_weight_lbs: 12345.6,
        total_volume_cuft: 187.4,
        max_length_in: 192, // 16 ft
        piece_count: 148,
        shape_profile: 'LONG_LOAD',
        routable: true,
        priority: false,
        issues: ['1 line has no digital-twin geometry'],
        lines: [
          {
            product_id: 'p-1',
            sku: '2X6-16-SPF',
            name: '2x6x16 SPF',
            quantity: 96,
            unit_weight_lbs: 32.4,
            unit_length_in: 192,
            unit_width_in: 5.5,
            unit_height_in: 1.5,
            stackable: true,
            has_geometry: true,
            line_weight_lbs: 3110.4,
            line_volume_cuft: 88,
          },
          {
            product_id: 'p-2',
            sku: 'WRC-2X8-20',
            name: 'Western red cedar 2x8x20',
            quantity: 24,
            unit_weight_lbs: 39.2,
            unit_length_in: 240,
            unit_width_in: 7.25,
            unit_height_in: 1.5,
            stackable: false,
            has_geometry: false,
            line_weight_lbs: 940.8,
            line_volume_cuft: 44,
            dim_override: {
              length_in: 240,
              width_in: 8,
              height_in: 2,
              tolerance_pct: 15,
              source: 'AVERAGE',
            },
          },
        ],
      },
      {
        order_id: 'o-2',
        customer_name: 'Okanagan Decks',
        address: '910 Gordon Dr',
        lat: 49.86,
        lng: -119.47,
        total_weight_lbs: 4820.2,
        total_volume_cuft: 62.8,
        max_length_in: 96, // 8 ft
        piece_count: 40,
        shape_profile: 'COMPACT',
        routable: true,
        priority: true,
        issues: [],
        lines: [],
      },
    ],
    loads: [
      {
        vehicle_id: 'veh-1',
        vehicle_name: 'Freightliner M2 Flatbed',
        driver_id: 'd-1',
        driver_name: 'Marc T.',
        capacity_weight_lbs: 19000,
        total_weight_lbs: 12345.6,
        total_distance_mi: 18.42,
        total_duration_min: 47.6,
        bed: { length_in: 288, width_in: 96, height_in: 96 },
        stops: [
          {
            order_id: 'o-1',
            sequence: 1,
            lat: 49.87,
            lng: -119.45,
            address: '2200 Benvoulin Rd',
            customer_name: 'Ridgeview Framing',
            weight_lbs: 12346,
            priority: false,
          },
          {
            order_id: 'o-2',
            sequence: 2,
            lat: 49.86,
            lng: -119.47,
            address: '910 Gordon Dr',
            customer_name: 'Okanagan Decks',
            weight_lbs: 4820,
            priority: true,
          },
        ],
        load_plan: {
          id: 'lp-1',
          gable_vehicle_id: 'veh-1',
          total_weight_lbs: 34210, // cargo + tare, per load.computeAxleLoads
          balance_score: 0.874,
          gvw_status: 'FAIL',
          unplaced: ['WRC-2X8-20'],
          max_load_height_in: 62.6,
          bed_volume_cuft: 942.1,
          usable_volume_cuft: 612.4,
          cargo_volume_cuft: 431.8,
          volume_utilization: 0.71,
          volume_status: 'WARN',
          created_at: '2026-08-11T15:00:00Z',
          axle_loads: [
            {
              axle_number: 1,
              weight_lbs: 11400,
              max_weight_lbs: 12000,
              utilization: 0.95,
              status: 'WARN',
            },
            {
              axle_number: 2,
              weight_lbs: 21730,
              max_weight_lbs: 21000,
              utilization: 1.035,
              status: 'FAIL',
            },
          ],
          placements: [
            {
              item_id: 'i-1',
              sku: '2X6-16-SPF',
              x: 0,
              y: 0,
              z: 0,
              length_in: 192,
              width_in: 5.5,
              height_in: 1.5,
              weight_lbs: 32.4,
              axle_group: 1,
              order_id: 'o-2',
              stop_sequence: 2,
              step: 1,
            },
            {
              item_id: 'i-2',
              sku: '2X6-16-SPF',
              x: 0,
              y: 5.5,
              z: 0,
              length_in: 192,
              width_in: 5.5,
              height_in: 1.5,
              weight_lbs: 32.4,
              axle_group: 1,
              order_id: 'o-2',
              stop_sequence: 2,
              step: 2,
            },
            {
              item_id: 'i-3',
              sku: 'WRC-2X8-20',
              x: 0,
              y: 0,
              z: 1.5,
              length_in: 240,
              width_in: 7.25,
              height_in: 1.5,
              weight_lbs: 39.2,
              axle_group: 2,
              order_id: 'o-1',
              stop_sequence: 1,
              step: 3,
            },
          ],
          securement: {
            cargo_weight_lbs: 20210,
            min_aggregate_wll_lbs: 10105,
            recommended_strap: '4" winch strap (WLL 5400 lb)',
            jurisdiction: 'CA_NSC',
            ruleset_name: 'Canada NSC Standard 10',
            rule_basis: 'NSC Standard 10 / provincial MTO',
            required_tie_downs: 3,
            anchor_spacing_in: 24,
            straps: [
              { number: 1, position_in: 24, over_height_in: 42.4, required_wll_lbs: 3369 },
              { number: 2, position_in: 120, over_height_in: 42.4, required_wll_lbs: 3369 },
              { number: 3, position_in: 216, over_height_in: 18.2, required_wll_lbs: 3369 },
            ],
            notes: ['Use edge protectors wherever a strap crosses a board edge.'],
          },
        },
        compliance: {
          status: 'PASS',
          flags: [],
          actions: [],
          checked_gross_lbs: 34210,
          checked_max_axle_lbs: 21730,
          checked_height_in: 162.4,
        },
        proof: {
          attachments: [
            {
              url: 'https://cdn.example.test/load-veh-1.jpg',
              kind: 'PHOTO',
              added_at: '2026-08-11T15:10:00Z',
            },
          ],
          signed_off: true,
          signed_by: 'Yard staff',
          signed_at: '2026-08-11T15:12:00Z',
        },
      },
      {
        vehicle_id: 'veh-2',
        vehicle_name: 'International Box Truck',
        driver_name: 'Priya S.',
        capacity_weight_lbs: 13500,
        total_weight_lbs: 4820.2,
        total_distance_mi: 9.05,
        total_duration_min: 26.2,
        bed: { length_in: 312, width_in: 100, height_in: 102 },
        stops: [
          {
            order_id: 'o-2',
            sequence: 1,
            lat: 49.86,
            lng: -119.47,
            address: '910 Gordon Dr',
            customer_name: 'Okanagan Decks',
            weight_lbs: 4820,
            priority: true,
          },
        ],
        load_plan: {
          id: 'lp-2',
          gable_vehicle_id: 'veh-2',
          total_weight_lbs: 17320,
          balance_score: 0.93,
          gvw_status: 'PASS',
          unplaced: [],
          max_load_height_in: 21.4,
          axle_loads: [
            {
              axle_number: 1,
              weight_lbs: 6100,
              max_weight_lbs: 10000,
              utilization: 0.61,
              status: 'PASS',
            },
          ],
          placements: [],
          created_at: '2026-08-11T15:00:00Z',
        },
        compliance: {
          status: 'WARN',
          flags: [
            {
              point: {
                id: 'rp-1',
                name: 'Bennett Bridge (W.R. Bennett)',
                lat: 49.8845,
                lng: -119.496,
                restriction_type: 'SEASONAL',
                notes: 'Floating bridge',
                created_at: '2026-08-01T00:00:00Z',
                updated_at: '2026-08-01T00:00:00Z',
              },
              distance_mi: 0.21,
              violation: 'seasonal restriction on route — verify before dispatch',
              severity: 'WARN',
            },
          ],
          actions: [
            {
              type: 'LOAD_ADJUST',
              description: 'Moved the heaviest stop to Freightliner M2 Flatbed',
              resolved: true,
            },
          ],
          checked_gross_lbs: 17320,
          checked_max_axle_lbs: 6100,
          checked_height_in: 21.4,
        },
        proof: {
          attachments: [
            {
              url: 'https://cdn.example.test/load-veh-2.jpg',
              kind: 'PHOTO',
              added_at: '2026-08-11T15:11:00Z',
            },
          ],
          signed_off: true,
          signed_by: 'Yard staff',
        },
      },
    ],
    unassigned_orders: [],
    created_at: '2026-08-11T14:00:00Z',
    updated_at: '2026-08-11T15:12:00Z',
  };
}

async function mountPlan(overrides: (p: WorkflowPlan) => void = () => {}): Promise<PlanWorkflow> {
  const p = plan();
  overrides(p);
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(p)));
  return mount<PlanWorkflow>('ailm-plan-workflow');
}

beforeEach(() => {
  window.history.replaceState({}, '', '/plan');
});

describe('PlanWorkflow — plan load', () => {
  it('opens the latest plan on the furthest step its status allows', async () => {
    const el = await mountPlan();
    // REVIEWED (rank 4) unlocks step 5, the manifest/push step.
    expect(text(el.querySelector('header'))).toContain('REVIEWED');
    expect(text(el)).toContain('Final review: 2 truck(s), 3 stop(s) on 2026-08-12.');
  });

  it('starts at ingest when no plan exists for the date', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({}, 404)));
    const el = await mount<PlanWorkflow>('ailm-plan-workflow');

    expect(text(el)).toContain(
      "Pick the delivery date and ingest its confirmed GableLBM orders to begin planning.",
    );
  });
});

describe('PlanWorkflow — step 1 order analysis', () => {
  it('totals the day in pounds and cubic feet', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Ingest & Analyze');

    // 12345.6 + 4820.2 = 17165.8 lb; 187.4 + 62.8 = 250.2 ft³.
    expect(text(el)).toContain(
      `2 order(s) analyzed — ${grouped(17166)} lb · ${grouped(250)} ft³`,
    );
  });

  it('converts the longest piece from inches to feet on each order card', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Ingest & Analyze');

    const cards = Array.from(el.querySelectorAll('.glass-card'));
    const ridgeview = cards.find((c) => text(c).includes('Ridgeview Framing'))!;
    // The stat grid is <dt>label</dt><dd>value</dd> with no whitespace between,
    // so the collapsed text reads "Weight12,346 lb".
    const body = text(ridgeview);

    expect(body).toContain(`Weight${grouped(12346)} lb`); // 12345.6 rounded
    expect(body).toContain('Volume187 ft³');
    expect(body).toContain('Pieces148');
    expect(body).toContain('Max len16 ft'); // 192 in / 12
    expect(body).toContain('LONG LOAD');
  });

  it('renders per-line weights and flags a line with no digital-twin geometry', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Ingest & Analyze');

    const cards = Array.from(el.querySelectorAll('.glass-card'));
    const ridgeview = cards.find((c) => text(c).includes('Ridgeview Framing'))!;

    expect(text(ridgeview)).toContain(`×96 ${grouped(3110)} lb`); // 3110.4 rounded
    expect(ridgeview.querySelector('[title="no digital-twin geometry"]')).not.toBeNull();
    expect(text(ridgeview)).toContain('1 line has no digital-twin geometry');
  });

  it('shows an applied dimension override with its tolerance and source', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Ingest & Analyze');

    expect(text(el)).toContain('240×8×2″ +15% (AVERAGE)');
  });
});

describe('PlanWorkflow — step 3 load safety readouts', () => {
  it('labels an over-GVW truck and reports gross weight and load height', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Pack Loads');

    const body = text(el);
    expect(body).toContain('GVW FAIL — overweight; redistribute or remove load');
    // total_weight_lbs is cargo + tare (load/solver.go), hence "gross".
    expect(body).toContain(`${grouped(34210)} lb gross · load 63″ tall`); // 62.6 → 63
  });

  it('renders each axle against its rating and colours the bar by status', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Pack Loads');

    const axleCard = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
      text(c).startsWith('Axle Loads'),
    )!;
    expect(text(axleCard)).toContain(`Axle 1 ${grouped(11400)} / ${grouped(12000)} lb`);
    expect(text(axleCard)).toContain(`Axle 2 ${grouped(21730)} / ${grouped(21000)} lb`);

    const bars = Array.from(axleCard.querySelectorAll('.h-full')) as HTMLElement[];
    expect(bars[0].className).toContain('bg-amber-warn'); // WARN
    expect(bars[1].className).toContain('bg-safety-red'); // FAIL
    expect(barWidth(bars[0])).toBeCloseTo(95, 6);
    expect(barWidth(bars[1])).toBeCloseTo(103.5, 6);
  });

  it('clamps a grossly overweight axle bar so it stays inside its track', async () => {
    const el = await mountPlan((p) => {
      p.loads[0].load_plan!.axle_loads[1].weight_lbs = 31500;
      p.loads[0].load_plan!.axle_loads[1].utilization = 1.5;
    });
    await clickByText(el, 'button', 'Pack Loads');

    const axleCard = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
      text(c).startsWith('Axle Loads'),
    )!;
    const bars = Array.from(axleCard.querySelectorAll('.h-full')) as HTMLElement[];

    // The bar caps at 120%, but the numbers must still tell the truth.
    expect(barWidth(bars[1])).toBe(120);
    expect(text(axleCard)).toContain(`Axle 2 ${grouped(31500)} / ${grouped(21000)} lb`);
  });

  it('reports balance score and the bed volume budget as percentages', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Pack Loads');

    const axleCard = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
      text(c).startsWith('Axle Loads'),
    )!;
    expect(text(axleCard)).toContain('Balance score 87%'); // 0.874
    expect(text(axleCard)).toContain('Bed volume 432 / 612 ft³ (71%)');
  });

  it('warns about cargo the packer could not place', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Pack Loads');

    expect(text(el)).toContain('Did not fit: WRC-2X8-20');
  });

  it('converts strap positions to feet and cites the jurisdiction ruleset', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Pack Loads');

    const secCard = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
      text(c).startsWith('Securement'),
    )!;
    const body = text(secCard);

    expect(body).toContain('3 tie-down(s) · 4" winch strap (WLL 5400 lb)');
    expect(body).toContain('rule min 3');
    expect(body).toContain('24″ anchor pitch');
    expect(body).toContain('Ruleset: Canada NSC Standard 10');
    expect(body).toContain('2.0 ft from nose'); // 24 in
    expect(body).toContain('10.0 ft from nose'); // 120 in
    expect(body).toContain('18.0 ft from nose'); // 216 in
    expect(body).toContain(`over 42″ · WLL ${grouped(3369)} lb`);
    expect(body).toContain(
      `Required aggregate WLL ${grouped(10105)} lb (50% of ${grouped(20210)} lb)`,
    );
    expect(secCard.querySelector('[title^="NSC Standard 10"]')?.textContent?.trim()).toBe('CA_NSC');
  });

  it('offers packing playback over every placed piece', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Pack Loads');

    const slider = el.querySelector('input[type="range"]') as HTMLInputElement;
    expect(slider.max).toBe('3');
    expect(text(el)).toContain('3 / 3 pcs');
  });

  it('marks a truck ready only once it has proof and a sign-off', async () => {
    const el = await mountPlan((p) => {
      p.loads[0].proof = { attachments: [], signed_off: false };
    });
    await clickByText(el, 'button', 'Pack Loads');

    const proofCard = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
      text(c).startsWith('Yard proof'),
    )!;
    expect(text(proofCard)).toContain('PENDING');
    const signOff = Array.from(proofCard.querySelectorAll('button')).find((b) =>
      text(b).includes('Sign off'),
    ) as HTMLButtonElement;
    expect(signOff.disabled).toBe(true);
    expect(signOff.title).toBe('Attach at least one photo/video first');
  });
});

describe('PlanWorkflow — step 4 compliance review', () => {
  it('states the checked load profile in both inches and feet', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Route Review');

    const profileCard = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
      text(c).startsWith('Checked Load Profile'),
    )!;
    const body = text(profileCard);

    expect(body).toContain(`Gross weight${grouped(34210)} lb`);
    expect(body).toContain(`Heaviest axle${grouped(21730)} lb`);
    expect(body).toContain('Travel height162″ (13.5 ft)'); // 162.4 in
  });

  it('names the truck in the clear-route banner', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Route Review');

    expect(text(el)).toContain(
      'Freightliner M2 Flatbed: route clear — no restricted-point violations',
    );
  });

  it('lists remaining flags and resolved AI actions for a WARN truck', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Route Review');
    await clickByText(el, 'button', 'International Box Truck');

    const body = text(el);
    expect(body).toContain(
      'International Box Truck: advisory flags on route — review before dispatch',
    );
    expect(body).toContain('WARN Bennett Bridge (W.R. Bennett) 0.21 mi');
    expect(body).toContain('seasonal restriction on route — verify before dispatch');
    expect(body).toContain('LOAD_ADJUST');
  });
});

describe('PlanWorkflow — T2-3 locked run', () => {
  it('shows the lock window and offers an unlock', async () => {
    const el = await mountPlan((p) => {
      p.lock = { locked: true, window: 'MORNING', lock_at: '06:00', reason: 'Scheduled lock' };
    });

    const body = text(el);
    expect(body).toContain('Run locked');
    expect(body).toContain('MORNING · 06:00');
    expect(body).toContain('Scheduled lock');
    expect(body).toContain('Unlock');
    expect(body).toContain('Locked — late adds need approval before they reshuffle the run.');
  });

  it('turns a 423 into an approval prompt and re-sends the reshuffle authorised', async () => {
    localStorage.setItem('ailm_name', 'Dana R.');
    const locked = plan();
    locked.lock = { locked: true, window: 'MORNING' };
    const assigned = { ...locked, status: 'ASSIGNED' as const };
    const attempts: { override: boolean; approved_by: string }[] = [];

    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init: RequestInit = {}) => {
        if (!url.endsWith('/assign')) return Promise.resolve(jsonResponse(locked));
        const body = JSON.parse(init.body as string);
        attempts.push(body);
        return Promise.resolve(
          body.override
            ? jsonResponse(assigned)
            : jsonResponse(
                { error: { code: 'locked', message: 'run is locked — manual approval required' } },
                423,
              ),
        );
      }),
    );
    const el = await mount<PlanWorkflow>('ailm-plan-workflow');

    await clickByText(el, 'button', 'Assign Trucks');
    await clickByText(el, 'button', 'Re-run assignment');

    expect(text(el)).toContain('run is locked — manual approval required');
    expect(attempts).toEqual([{ override: false, approved_by: '' }]);

    await clickByText(el, 'button', 'Approve & override');

    // The approver is recorded from the signed-in staff name, not anonymised.
    expect(attempts[1]).toEqual({ override: true, approved_by: 'Dana R.' });
    expect(text(el)).not.toContain('Approve & override');
  });

  it('reports a plain failure without offering an override', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) =>
        Promise.resolve(
          url.endsWith('/assign')
            ? jsonResponse(
                { error: { code: 'upstream', message: 'GableLBM fleet unavailable' } },
                502,
              )
            : jsonResponse(plan()),
        ),
      ),
    );
    const el = await mount<PlanWorkflow>('ailm-plan-workflow');

    await clickByText(el, 'button', 'Assign Trucks');
    await clickByText(el, 'button', 'Re-run assignment');

    expect(text(el)).toContain('GableLBM fleet unavailable');
    expect(text(el)).not.toContain('Approve & override');
  });
});

describe('PlanWorkflow — step 5 manifest and push', () => {
  it('summarises each truck with distance, drive time and piece count', async () => {
    const el = await mountPlan();

    const manifest = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
      text(c).includes('Freightliner M2 Flatbed'),
    )!;
    const body = text(manifest);

    expect(body).toContain(`Driver: Marc T. · ${grouped(12346)} lb · 18.4 mi · 48 min · 3 pcs`);
    expect(body).toContain('SIGNED');
  });

  it('blocks the push while a truck still FAILs route compliance', async () => {
    const el = await mountPlan((p) => {
      p.loads[1].compliance!.status = 'FAIL';
    });

    const push = Array.from(el.querySelectorAll('button')).find((b) =>
      text(b).includes('Push to GableLBM dispatch'),
    ) as HTMLButtonElement;

    expect(push.disabled).toBe(true);
    expect(push.title).toBe('Resolve all FAIL compliance flags before pushing');
    expect(text(el)).toContain('One or more trucks still FAIL route compliance');
  });

  it('blocks the push until every truck has yard proof and a sign-off', async () => {
    const el = await mountPlan((p) => {
      p.loads[1].proof = { attachments: [], signed_off: false };
    });

    const push = Array.from(el.querySelectorAll('button')).find((b) =>
      text(b).includes('Push to GableLBM dispatch'),
    ) as HTMLButtonElement;

    expect(push.disabled).toBe(true);
    expect(text(el)).toContain(
      'Yard proof + sign-off required before depart: International Box Truck.',
    );
  });

  // The push gate must judge each truck's OWN capacity, not just its route: a
  // truck whose load plan came back `gvw_status: 'FAIL'` — over its GVWR or an
  // axle rating — must not be one click from the dispatch board merely because
  // its route crosses no restricted point. The UI mirrors the backend gate
  // (internal/workflow/service.go blockingCapacityReasons /
  // capacityStatusClears) and mirrors its WHITELIST semantics, so a status
  // value neither side has seen before fails closed on both.
  it('blocks the push while a truck is over its own GVW/axle rating', async () => {
    const el = await mountPlan(); // loads[0].load_plan.gvw_status === 'FAIL'

    const push = Array.from(el.querySelectorAll('button')).find((b) =>
      text(b).includes('Push to GableLBM dispatch'),
    ) as HTMLButtonElement;

    expect(push.disabled).toBe(true);
  });

  // `load_plan.unplaced` lists SKUs the packer could not physically fit. The
  // step-5 manifest — the sheet the yard and the ERP receive — lists the ORDERED
  // lines and quantities, so without an explicit dropped-cargo callout a truck
  // goes out with fewer pieces than the paperwork claims and nothing says so.
  // The backend manifest (workflow buildManifest) emits `unplaced`; this is the
  // UI half of the same statement.
  it('flags dropped cargo on the manifest', async () => {
    const el = await mountPlan();

    const manifest = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
      text(c).includes('Freightliner M2 Flatbed'),
    )!;

    expect(text(manifest).toLowerCase()).toMatch(/did not fit|unplaced|not loaded/);
  });

  it('sends the push and re-renders the plan as live once it lands', async () => {
    const p = plan();
    // The push gate refuses a truck whose OWN capacity is not cleared, and the
    // shared fixture deliberately ships one that is not (gvw FAIL, axle 2 FAIL,
    // a dropped SKU). A plan that actually reaches the dispatch board has
    // cleared all three, so this fixture clears them.
    p.loads[0].load_plan!.gvw_status = 'PASS';
    p.loads[0].load_plan!.axle_loads[1].status = 'PASS';
    p.loads[0].load_plan!.unplaced = [];
    const pushed = { ...p, status: 'PUSHED' as const };
    const fetchMock = vi.fn((_url: string, init: RequestInit = {}) =>
      Promise.resolve(jsonResponse((init.method ?? 'GET') === 'POST' ? pushed : p)),
    );
    vi.stubGlobal('fetch', fetchMock);
    const el = await mount<PlanWorkflow>('ailm-plan-workflow');

    await clickByText(el, 'button', 'Push to GableLBM dispatch');

    const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(url).toBe('/api/v1/workflow/plans/plan-1/push');
    expect(init.method).toBe('POST');
    expect(text(el)).toContain('Pushed to GableLBM');
  });

  it('confirms the push once the plan is live on the dispatch board', async () => {
    const el = await mountPlan((p) => {
      p.status = 'PUSHED';
    });

    expect(text(el)).toContain('Pushed to GableLBM — routes are on the Dispatch Board');
    expect(
      Array.from(el.querySelectorAll('button')).some((b) =>
        text(b).includes('Push to GableLBM dispatch'),
      ),
    ).toBe(false);
  });
});
