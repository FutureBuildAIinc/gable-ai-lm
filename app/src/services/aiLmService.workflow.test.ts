// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * The rest of the AI_LM integration contract.
 *
 * `aiLmService.test.ts` pins a representative slice; this file finishes the
 * job for the guided-workflow, load, routing and compliance endpoints. Every
 * URL, verb and payload here is hand-duplicated from the Go handlers with no
 * OpenAPI document or shared schema in between, so a rename on either side is
 * invisible until a dispatcher hits it. These tests are the only thing standing
 * between that and production.
 *
 * They exercise the real `fetchWithAuth`, stubbing only `fetch`, so the
 * transport's header injection and error unwrapping are covered too.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { aiLmService, type LoadPlan, type OptimizeRequest } from './aiLmService.ts';
import { jsonResponse } from '../test/dom.ts';

let fetchMock: ReturnType<typeof vi.fn>;

function lastCall() {
  const [url, init] = fetchMock.mock.calls[fetchMock.mock.calls.length - 1] as [
    string,
    RequestInit,
  ];
  return {
    url,
    method: init.method ?? 'GET',
    body: init.body ? JSON.parse(init.body as string) : undefined,
  };
}

beforeEach(() => {
  // A `Response` body can only be read once, and several tests below make more
  // than one call, so build a fresh one per call rather than resolving a shared
  // instance.
  fetchMock = vi.fn(() => Promise.resolve(jsonResponse({})));
  vi.stubGlobal('fetch', fetchMock);
});

describe('aiLmService — fleet and catalog', () => {
  it('scopes a profile read to the GableLBM vehicle id', async () => {
    await aiLmService.getProfile('veh-7');
    expect(lastCall()).toMatchObject({ url: '/api/v1/fleet/profiles/veh-7', method: 'GET' });
  });

  it('reads the AI_LM dimension overrides separately from the resolved catalog', async () => {
    await aiLmService.listDimensions();
    expect(lastCall().url).toBe('/api/v1/catalog/dimensions');

    await aiLmService.listProducts();
    expect(lastCall().url).toBe('/api/v1/catalog/products');
  });
});

describe('aiLmService — load solver', () => {
  it('POSTs the vehicle and every line item to the optimizer', async () => {
    const req: OptimizeRequest = {
      vehicle_id: 'veh-1',
      route_id: 'route-3',
      items: [
        {
          product_id: 'p-1',
          sku: '2X6-16-SPF',
          quantity: 96,
          length_in: 192,
          width_in: 5.5,
          height_in: 1.5,
          weight_lbs: 32.4,
          stackable: true,
        },
      ],
    };

    await aiLmService.optimizeLoad(req);

    expect(lastCall()).toEqual({ url: '/api/v1/load/optimize', method: 'POST', body: req });
  });

  it('carries the non-stackable flag through untouched', async () => {
    // A crated window or a stone slab that arrives as `stackable: true` is a
    // crushed product, so this flag must survive serialisation verbatim.
    await aiLmService.optimizeLoad({
      vehicle_id: 'veh-1',
      items: [
        {
          product_id: 'p-9',
          sku: 'SLAB-GRANITE-3CM',
          quantity: 2,
          length_in: 120,
          width_in: 78,
          height_in: 1.25,
          weight_lbs: 810,
          stackable: false,
        },
      ],
    });

    expect(lastCall().body.items[0].stackable).toBe(false);
  });

  it('reads a persisted plan back by id', async () => {
    await aiLmService.getLoadPlan('lp-4');
    expect(lastCall()).toMatchObject({ url: '/api/v1/load/lp-4', method: 'GET' });
  });

  it('decodes an UNKNOWN axle status without coercing it to a pass', async () => {
    // The four-valued enum only reaches the UI intact if the client stops
    // narrowing it to PASS/WARN/FAIL — see load/model.go StatusUnknown.
    fetchMock.mockResolvedValue(
      jsonResponse({
        id: 'lp-4',
        gable_vehicle_id: 'veh-1',
        placements: [],
        total_weight_lbs: 21_400,
        balance_score: 0,
        gvw_status: 'FAIL',
        unplaced: [],
        profile_status: 'INCOMPLETE',
        profile_issues: ['axle 2 has no rated capacity — its load cannot be judged'],
        created_at: '2026-08-11T15:00:00Z',
        axle_loads: [
          {
            axle_number: 2,
            weight_lbs: 21_400,
            max_weight_lbs: 0,
            utilization: 0,
            status: 'UNKNOWN',
            advisory: true,
          },
        ],
      }),
    );

    const plan: LoadPlan = await aiLmService.getLoadPlan('lp-4');

    expect(plan.axle_loads[0].status).toBe('UNKNOWN');
    expect(plan.axle_loads[0].advisory).toBe(true);
    expect(plan.profile_status).toBe('INCOMPLETE');
    expect(plan.profile_issues).toEqual([
      'axle 2 has no rated capacity — its load cannot be judged',
    ]);
    // UNKNOWN is blocking, so the published three-value roll-up must be FAIL.
    expect(plan.gvw_status).toBe('FAIL');
  });
});

describe('aiLmService — routing', () => {
  it('POSTs a plan request with the depot coordinates it was given', async () => {
    await aiLmService.planRoute({
      date: '2026-08-12',
      branch_id: 'branch-1',
      depot_lat: 49.8863,
      depot_lng: -119.4666,
    });

    expect(lastCall()).toEqual({
      url: '/api/v1/routing/plan',
      method: 'POST',
      body: {
        date: '2026-08-12',
        branch_id: 'branch-1',
        depot_lat: 49.8863,
        depot_lng: -119.4666,
      },
    });
  });

  it('omits absent optional fields rather than sending nulls', async () => {
    // A null branch_id is not the same request as "no branch filter"; JSON.stringify
    // drops undefined keys, and this pins that we rely on that.
    await aiLmService.planRoute({ date: '2026-08-12' });
    expect(lastCall().body).toEqual({ date: '2026-08-12' });
  });

  it('reads and approves a route plan by id', async () => {
    await aiLmService.getRoutePlan('rp-2');
    expect(lastCall()).toMatchObject({ url: '/api/v1/routing/plan/rp-2', method: 'GET' });

    await aiLmService.approveRoutePlan('rp-2');
    expect(lastCall()).toMatchObject({
      url: '/api/v1/routing/plan/rp-2/approve',
      method: 'POST',
      body: undefined,
    });
  });
});

describe('aiLmService — compliance registry', () => {
  it('lists restricted points', async () => {
    await aiLmService.listRestrictedPoints();
    expect(lastCall()).toMatchObject({
      url: '/api/v1/compliance/restricted-points',
      method: 'GET',
    });
  });

  it('POSTs a new restricted point with only the limits that were set', async () => {
    await aiLmService.createRestrictedPoint({
      name: 'Bennett Bridge',
      lat: 49.8845,
      lng: -119.496,
      restriction_type: 'WEIGHT',
      max_gross_weight_lbs: 40_000,
      notes: 'Floating bridge',
    });

    const { url, method, body } = lastCall();
    expect(url).toBe('/api/v1/compliance/restricted-points');
    expect(method).toBe('POST');
    expect(body.max_gross_weight_lbs).toBe(40_000);
    // An unset clearance must not be posted as a 0-inch clearance.
    expect(body).not.toHaveProperty('max_height_in');
  });
});

describe('aiLmService — guided workflow lifecycle', () => {
  it('ingests a day by date', async () => {
    await aiLmService.ingestWorkflow('2026-08-12');
    expect(lastCall()).toEqual({
      url: '/api/v1/workflow/plans',
      method: 'POST',
      body: { date: '2026-08-12' },
    });
  });

  it('reads a plan by id', async () => {
    await aiLmService.getWorkflow('plan-1');
    expect(lastCall()).toMatchObject({ url: '/api/v1/workflow/plans/plan-1', method: 'GET' });
  });

  it('walks pack, review and push as bodyless POSTs', async () => {
    await aiLmService.packWorkflow('plan-1');
    expect(lastCall()).toMatchObject({
      url: '/api/v1/workflow/plans/plan-1/pack',
      method: 'POST',
      body: undefined,
    });

    await aiLmService.reviewWorkflow('plan-1');
    expect(lastCall().url).toBe('/api/v1/workflow/plans/plan-1/review');

    await aiLmService.pushWorkflow('plan-1');
    expect(lastCall().url).toBe('/api/v1/workflow/plans/plan-1/push');
  });

  it('fetches the AI briefing for a plan', async () => {
    await aiLmService.getBriefing('plan-1');
    expect(lastCall()).toMatchObject({
      url: '/api/v1/workflow/plans/plan-1/briefing',
      method: 'GET',
    });
  });
});

describe('aiLmService — stop priority and dimension overrides', () => {
  it('PUTs a stop priority under the plan and order', async () => {
    await aiLmService.setStopPriority('plan-1', 'o-3', true);
    expect(lastCall()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/stops/o-3/priority',
      method: 'PUT',
      body: { priority: true, override: false, approved_by: '' },
    });
  });

  it('carries an approver when a priority change overrides a locked run', async () => {
    await aiLmService.setStopPriority('plan-1', 'o-3', false, true, 'Dana R.');
    expect(lastCall().body).toEqual({
      priority: false,
      override: true,
      approved_by: 'Dana R.',
    });
  });

  it('PUTs a per-order dimension override with its tolerance and provenance', async () => {
    await aiLmService.setLineDimensions('plan-1', 'o-1', {
      product_id: 'p-2',
      sku: 'WRC-2X8-20',
      length_in: 240,
      width_in: 8,
      height_in: 2,
      tolerance_pct: 15,
      source: 'AVERAGE',
    });

    expect(lastCall()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/orders/o-1/dimensions',
      method: 'PUT',
      body: {
        product_id: 'p-2',
        sku: 'WRC-2X8-20',
        length_in: 240,
        width_in: 8,
        height_in: 2,
        tolerance_pct: 15,
        source: 'AVERAGE',
      },
    });
  });
});

describe('aiLmService — yard proof and sign-off', () => {
  it('POSTs an attachment under the plan and vehicle', async () => {
    await aiLmService.attachProof('plan-1', 'veh-2', {
      url: 'https://cdn.example.test/load.jpg',
      kind: 'PHOTO',
      added_by: 'Dana R.',
    });

    expect(lastCall()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/loads/veh-2/proof',
      method: 'POST',
      body: {
        url: 'https://cdn.example.test/load.jpg',
        kind: 'PHOTO',
        added_by: 'Dana R.',
      },
    });
  });

  it('records who signed a load off', async () => {
    await aiLmService.signOffLoad('plan-1', 'veh-2', { signed_by: 'Dana R.', role: 'YARD' });
    expect(lastCall()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/loads/veh-2/sign-off',
      method: 'POST',
      body: { signed_by: 'Dana R.', role: 'YARD' },
    });
  });
});

describe('aiLmService — lock windows and late adds', () => {
  it('POSTs a scheduled lock with its window and the person setting it', async () => {
    await aiLmService.lockPlan('plan-1', {
      locked: true,
      window: 'MORNING',
      lock_at: '06:00',
      locked_by: 'Dana R.',
    });

    expect(lastCall()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/lock',
      method: 'POST',
      body: { locked: true, window: 'MORNING', lock_at: '06:00', locked_by: 'Dana R.' },
    });
  });

  it('sends an unlock reason and defaults both fields to empty strings', async () => {
    await aiLmService.unlockPlan('plan-1');
    expect(lastCall()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/unlock',
      method: 'POST',
      body: { reason: '', locked_by: '' },
    });

    await aiLmService.unlockPlan('plan-1', 'manual unlock', 'Dana R.');
    expect(lastCall().body).toEqual({ reason: 'manual unlock', locked_by: 'Dana R.' });
  });

  it('queues a late order against the plan', async () => {
    await aiLmService.addLateOrder('plan-1', { order_id: 'o-9', requested_by: 'Dana R.' });
    expect(lastCall()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/late-adds',
      method: 'POST',
      body: { order_id: 'o-9', requested_by: 'Dana R.' },
    });
  });

  it('distinguishes approving a late add from rejecting one', async () => {
    await aiLmService.resolveLateAdd('plan-1', 'o-9', { reject: false, approved_by: 'Dana R.' });
    expect(lastCall()).toMatchObject({
      url: '/api/v1/workflow/plans/plan-1/late-adds/o-9/resolve',
      method: 'POST',
      body: { reject: false, approved_by: 'Dana R.' },
    });

    await aiLmService.resolveLateAdd('plan-1', 'o-9', { reject: true, approved_by: 'Dana R.' });
    expect(lastCall().body.reject).toBe(true);
  });
});

describe('aiLmService — auth', () => {
  it('POSTs only the email to the staff-login endpoint', async () => {
    await aiLmService.login('dana@example.test');
    expect(lastCall()).toEqual({
      url: '/api/v1/auth/login',
      method: 'POST',
      body: { email: 'dana@example.test' },
    });
  });

  it('returns the token and roles the backend issued', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ token: 'jwt-abc', name: 'Dana R.', roles: ['dispatcher', 'yard'] }),
    );

    await expect(aiLmService.login('dana@example.test')).resolves.toEqual({
      token: 'jwt-abc',
      name: 'Dana R.',
      roles: ['dispatcher', 'yard'],
    });
  });

  it('rejects an unentitled staff account with the backend message', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ error: { code: 'forbidden', message: 'no AI_LM entitlement' } }, 403),
    );

    await expect(aiLmService.login('nope@example.test')).rejects.toThrow('no AI_LM entitlement');
  });
});

describe('aiLmService — path construction', () => {
  it('percent-encodes the one value it puts in a query string', async () => {
    await aiLmService.latestWorkflow('2026-08-12 09:00');
    expect(lastCall().url).toBe('/api/v1/workflow/plans/latest?date=2026-08-12%2009%3A00');
  });

  it('interpolates path segments verbatim (characterization)', async () => {
    // Every id the service puts in a PATH is template-interpolated raw, unlike
    // the date above. Today's ids are backend-issued UUIDs so nothing escapes,
    // but the asymmetry is worth knowing about before anyone routes a
    // user-supplied identifier (a GableLBM order number, say) through here.
    await aiLmService.getWorkflow('plan 1/../2');
    expect(lastCall().url).toBe('/api/v1/workflow/plans/plan 1/../2');
  });
});
