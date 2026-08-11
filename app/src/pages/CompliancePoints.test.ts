// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * The restricted-point registry is the dealer's own list of bridges, culverts
 * and low overpasses. Its limits are compared against a load's gross weight,
 * heaviest axle and travel height, so the summary column must never imply a
 * limit that was not entered.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { CompliancePoints } from './CompliancePoints.ts';
import './CompliancePoints.ts';
import type { RestrictedPoint, RestrictedPointInput } from '../services/aiLmService.ts';
import { clickByText, jsonResponse, mount, setValue, text } from '../test/dom.ts';

const grouped = (n: number) => n.toLocaleString();

function point(over: Partial<RestrictedPoint>): RestrictedPoint {
  return {
    id: 'rp-x',
    name: 'Unnamed',
    lat: 49.8845,
    lng: -119.496,
    restriction_type: 'WEIGHT',
    notes: '',
    created_at: '2026-08-01T00:00:00Z',
    updated_at: '2026-08-01T00:00:00Z',
    ...over,
  };
}

let created: RestrictedPointInput | null = null;

function stubComplianceApi(list: RestrictedPoint[]) {
  created = null;
  const fetchMock = vi.fn((_url: string, init: RequestInit = {}) => {
    if ((init.method ?? 'GET') === 'POST') {
      created = JSON.parse(init.body as string) as RestrictedPointInput;
      return Promise.resolve(jsonResponse(point({ id: 'rp-new', ...created })));
    }
    return Promise.resolve(jsonResponse(list));
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function rowFor(el: CompliancePoints, name: string): HTMLTableRowElement {
  const row = Array.from(el.querySelectorAll('tbody tr')).find((r) => text(r).includes(name));
  if (!row) throw new Error(`no row for "${name}"`);
  return row as HTMLTableRowElement;
}

beforeEach(() => {
  window.history.replaceState({}, '', '/compliance');
});

describe('CompliancePoints — limit summary', () => {
  it('prefers the gross-weight limit, then axle, then clearance', async () => {
    stubComplianceApi([
      point({
        id: 'rp-1',
        name: 'Bennett Bridge',
        max_gross_weight_lbs: 21000,
        max_axle_weight_lbs: 18000,
      }),
      point({ id: 'rp-2', name: 'McCulloch Culvert', max_axle_weight_lbs: 18000 }),
      point({
        id: 'rp-3',
        name: 'CN Overpass',
        restriction_type: 'HEIGHT',
        max_height_in: 136,
      }),
    ]);
    const el = await mount<CompliancePoints>('ailm-compliance-points');

    expect(text(rowFor(el, 'Bennett Bridge'))).toContain(`${grouped(21000)} lb gross`);
    expect(text(rowFor(el, 'McCulloch Culvert'))).toContain(`${grouped(18000)} lb/axle`);
    expect(text(rowFor(el, 'CN Overpass'))).toContain('136" clearance');
  });

  it('shows a dash rather than inventing a limit for a seasonal restriction', async () => {
    stubComplianceApi([
      point({ id: 'rp-4', name: 'Gallagher Rd', restriction_type: 'SEASONAL' }),
    ]);
    const el = await mount<CompliancePoints>('ailm-compliance-points');

    const row = text(rowFor(el, 'Gallagher Rd'));
    expect(row).toContain('SEASONAL');
    expect(row).toContain('—');
    expect(row).not.toContain('0 lb');
  });

  it('renders coordinates at the six-figure precision the geofence uses', async () => {
    stubComplianceApi([point({ id: 'rp-5', name: 'Bennett Bridge', lat: 49.88452, lng: -119.4960123 })]);
    const el = await mount<CompliancePoints>('ailm-compliance-points');

    expect(text(rowFor(el, 'Bennett Bridge'))).toContain('49.8845, -119.4960');
  });

  it('invites the operator to seed or add their own points when empty', async () => {
    stubComplianceApi([]);
    const el = await mount<CompliancePoints>('ailm-compliance-points');

    expect(text(el)).toContain('No restricted points.');
  });
});

describe('CompliancePoints — adding a point', () => {
  it('keeps Create disabled until the point is named', async () => {
    stubComplianceApi([]);
    const el = await mount<CompliancePoints>('ailm-compliance-points');
    await clickByText(el, 'button', 'Add Point');

    const create = Array.from(el.querySelectorAll('button')).find((b) =>
      text(b).includes('Create Point'),
    ) as HTMLButtonElement;
    expect(create.disabled).toBe(true);

    const nameField = Array.from(el.querySelectorAll('label')).find((l) =>
      text(l).startsWith('Name'),
    )!;
    await setValue(el, nameField.querySelector('input') as HTMLInputElement, 'Mission Creek Bridge');

    expect(
      (
        Array.from(el.querySelectorAll('button')).find((b) =>
          text(b).includes('Create Point'),
        ) as HTMLButtonElement
      ).disabled,
    ).toBe(false);
  });

  it('omits a blank limit instead of posting it as a zero limit', async () => {
    // A 0 lb limit would flag every load; an absent limit means "not weight
    // restricted". CompliancePoints._set maps '' to undefined for exactly this.
    stubComplianceApi([]);
    const el = await mount<CompliancePoints>('ailm-compliance-points');
    await clickByText(el, 'button', 'Add Point');

    const field = (label: string) =>
      Array.from(el.querySelectorAll('label'))
        .find((l) => text(l).startsWith(label))!
        .querySelector('input') as HTMLInputElement;

    await setValue(el, field('Name'), 'Mission Creek Bridge');
    await setValue(el, field('Latitude'), '49.8531');
    await setValue(el, field('Longitude'), '-119.4402');
    await setValue(el, field('Max height (in)'), '');
    await setValue(el, field('Max gross weight (lb)'), '24000');
    await clickByText(el, 'button', 'Create Point');

    expect(created).toMatchObject({
      name: 'Mission Creek Bridge',
      lat: 49.8531,
      lng: -119.4402,
      max_gross_weight_lbs: 24000,
    });
    expect(created!.max_height_in).toBeUndefined();
  });

  it('appends the created point to the registry and closes the form', async () => {
    stubComplianceApi([]);
    const el = await mount<CompliancePoints>('ailm-compliance-points');
    await clickByText(el, 'button', 'Add Point');

    const nameField = Array.from(el.querySelectorAll('label')).find((l) =>
      text(l).startsWith('Name'),
    )!;
    await setValue(el, nameField.querySelector('input') as HTMLInputElement, 'Mission Creek Bridge');
    await clickByText(el, 'button', 'Create Point');

    expect(text(rowFor(el, 'Mission Creek Bridge'))).toContain('WEIGHT');
    expect(text(el)).not.toContain('New Restricted Point');
  });

  it('keeps the form open and shows why a create was rejected', async () => {
    stubComplianceApi([]);
    vi.stubGlobal(
      'fetch',
      vi.fn((_url: string, init: RequestInit = {}) =>
        Promise.resolve(
          (init.method ?? 'GET') === 'POST'
            ? jsonResponse({ error: { code: 'invalid', message: 'lat/lng out of range' } }, 400)
            : jsonResponse([]),
        ),
      ),
    );
    const el = await mount<CompliancePoints>('ailm-compliance-points');
    await clickByText(el, 'button', 'Add Point');

    const nameField = Array.from(el.querySelectorAll('label')).find((l) =>
      text(l).startsWith('Name'),
    )!;
    await setValue(el, nameField.querySelector('input') as HTMLInputElement, 'Nowhere');
    await clickByText(el, 'button', 'Create Point');

    expect(text(el)).toContain('lat/lng out of range');
    expect(text(el)).toContain('New Restricted Point');
  });
});
