// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * The typed client is AI_LM's half of an unversioned integration contract: the
 * route shapes here are duplicated by hand from the Go handlers, with nothing
 * (no OpenAPI, no shared schema) keeping them in step. These tests pin the
 * URLs, verbs and payloads so a backend route rename fails here instead of at
 * a dispatcher's desk.
 *
 * They run against the real fetchWithAuth with only `fetch` stubbed, so the
 * transport's header injection is exercised too.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { aiLmService, ApiError, isConflict, type ProfileInput } from './aiLmService.ts';
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
  fetchMock = vi.fn().mockResolvedValue(jsonResponse({}));
  vi.stubGlobal('fetch', fetchMock);
});

describe('aiLmService — request routing', () => {
  it('GETs the fleet profile collection', async () => {
    await aiLmService.listProfiles();
    expect(lastCall()).toMatchObject({ url: '/api/v1/fleet/profiles', method: 'GET' });
  });

  it('PUTs the whole profile input to the vehicle-scoped path', async () => {
    const input: ProfileInput = {
      name: 'Freightliner M2 Flatbed',
      bed_length_in: 288,
      bed_width_in: 96,
      bed_height_in: 96,
      gvwr_lbs: 33000,
      tare_weight_lbs: 14000,
      axles: [
        { axle_number: 1, max_weight_lbs: 12000, position_from_front_in: 0, axle_type: 'STEER' },
        { axle_number: 2, max_weight_lbs: 21000, position_from_front_in: 240, axle_type: 'DRIVE' },
      ],
    };

    await aiLmService.upsertProfile('veh-7', input);

    expect(lastCall()).toEqual({
      url: '/api/v1/fleet/profiles/veh-7',
      method: 'PUT',
      body: input,
    });
  });

  it('URL-encodes the plan date on the latest-plan lookup', async () => {
    await aiLmService.latestWorkflow('2026-08-12');
    expect(lastCall().url).toBe('/api/v1/workflow/plans/latest?date=2026-08-12');
  });

  it('builds the nested resequence path and carries the lock override', async () => {
    await aiLmService.resequenceWorkflow('plan-1', 'veh-2', ['o-3', 'o-1'], true, 'Dana');

    expect(lastCall()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/loads/veh-2/sequence',
      method: 'PUT',
      body: { order_ids: ['o-3', 'o-1'], override: true, approved_by: 'Dana' },
    });
  });

  it('defaults the lock override to off so a normal reshuffle stays gated', async () => {
    await aiLmService.assignWorkflow('plan-1');
    expect(lastCall().body).toEqual({ override: false, approved_by: '' });
  });

  it('POSTs the compliance route check with the load profile intact', async () => {
    await aiLmService.checkRoute({
      route: [
        { lat: 49.88, lng: -119.49 },
        { lat: 49.86, lng: -119.45 },
      ],
      load: { gross_weight_lbs: 31500, max_axle_lbs: 20800, height_in: 138 },
      buffer_miles: 0.75,
    });

    const { url, method, body } = lastCall();
    expect(url).toBe('/api/v1/compliance/check-route');
    expect(method).toBe('POST');
    expect(body.load).toEqual({ gross_weight_lbs: 31500, max_axle_lbs: 20800, height_in: 138 });
    expect(body.route).toHaveLength(2);
  });

  it('pushes a plan with no request body', async () => {
    await aiLmService.pushWorkflow('plan-1');
    expect(lastCall()).toMatchObject({
      url: '/api/v1/workflow/plans/plan-1/push',
      method: 'POST',
      body: undefined,
    });
  });
});

describe('aiLmService — response handling', () => {
  it('returns the decoded catalog rows, geometry provenance included', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse([
        {
          gable_product_id: 'p-1',
          sku: '2X6-16-SPF',
          name: '2x6x16 SPF',
          length_in: 192,
          width_in: 5.5,
          height_in: 1.5,
          stackable: true,
          weight_lbs: 32.4,
          geometry_source: 'PIM',
          has_geometry: true,
        },
      ]),
    );

    const products = await aiLmService.listProducts();

    expect(products).toHaveLength(1);
    expect(products[0].geometry_source).toBe('PIM');
    expect(products[0].has_geometry).toBe(true);
  });

  it('surfaces the backend error envelope message, not the status code', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(
        {
          error: { code: 'upstream_unavailable', message: 'failed to load catalog from PIM' },
          meta: { request_id: 'req-9' },
        },
        502,
      ),
    );

    await expect(aiLmService.listProducts()).rejects.toThrow('failed to load catalog from PIM');
  });

  it('falls back to the status code when the error body is not JSON', async () => {
    fetchMock.mockResolvedValue(new Response('<html>502 Bad Gateway</html>', { status: 502 }));

    await expect(aiLmService.listProducts()).rejects.toThrow('HTTP 502');
  });

  it('falls back to the status code when the envelope carries no message', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ meta: { request_id: 'req-9' } }, 423));

    await expect(aiLmService.assignWorkflow('plan-1')).rejects.toThrow('HTTP 423');
  });

  // The optimistic lock in workflow.Repository returns 409 when a write loses
  // the race. The page has to tell that apart from every other failure — it is
  // the one case where the recovery is "reload", not "retry" or "override" —
  // so the status travels with the error instead of being guessed from wording.
  it('carries the HTTP status on the thrown error', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(
        { error: { code: 'conflict', message: 'workflow plan was modified concurrently' } },
        409,
      ),
    );

    await expect(aiLmService.packWorkflow('plan-1')).rejects.toBeInstanceOf(ApiError);
    await expect(aiLmService.packWorkflow('plan-1')).rejects.toMatchObject({ status: 409 });
  });

  it('identifies a lost optimistic-lock race as a conflict', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ error: { code: 'conflict', message: 'modified concurrently' } }, 409),
    );

    const err = await aiLmService.packWorkflow('plan-1').catch((e: unknown) => e);
    expect(isConflict(err)).toBe(true);
  });

  it('does not mistake a lock or a server error for a conflict', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ error: { code: 'locked', message: 'run is locked' } }, 423),
    );
    const locked = await aiLmService.packWorkflow('plan-1').catch((e: unknown) => e);
    expect(isConflict(locked)).toBe(false);

    fetchMock.mockResolvedValue(jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500));
    const boom = await aiLmService.packWorkflow('plan-1').catch((e: unknown) => e);
    expect(isConflict(boom)).toBe(false);
  });
});
