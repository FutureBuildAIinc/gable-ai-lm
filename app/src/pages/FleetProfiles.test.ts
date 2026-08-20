// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * The fleet profile editor is where GVWR, tare and per-axle ratings are set.
 * Every downstream PASS/WARN/FAIL the load solver produces is measured against
 * the numbers saved from this form, so what it sends matters as much as what it
 * shows.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { FleetProfiles } from './FleetProfiles.ts';
import './FleetProfiles.ts';
import type { ProfileInput, VehicleProfile } from '../services/aiLmService.ts';
import { clickByText, jsonResponse, mount, setValue, text } from '../test/dom.ts';

const grouped = (n: number) => n.toLocaleString();

function profiles(): VehicleProfile[] {
  return [
    {
      id: 'fp-1',
      gable_vehicle_id: '11111111-1111-1111-1111-111111111111',
      name: 'Freightliner M2 Flatbed',
      bed_length_in: 288,
      bed_width_in: 96,
      bed_height_in: 96,
      gvwr_lbs: 33000,
      tare_weight_lbs: 14000,
      axles: [
        {
          id: 'ax-1',
          axle_number: 1,
          max_weight_lbs: 12000,
          position_from_front_in: 0,
          axle_type: 'STEER',
        },
        {
          id: 'ax-2',
          axle_number: 2,
          max_weight_lbs: 21000,
          position_from_front_in: 240,
          axle_type: 'DRIVE',
        },
      ],
      created_at: '2026-08-01T00:00:00Z',
      updated_at: '2026-08-01T00:00:00Z',
    },
    {
      id: 'fp-2',
      gable_vehicle_id: '22222222-2222-2222-2222-222222222222',
      name: 'International Box Truck',
      bed_length_in: 312,
      bed_width_in: 100,
      bed_height_in: 102,
      gvwr_lbs: 26000,
      tare_weight_lbs: 12500,
      axles: [
        {
          id: 'ax-3',
          axle_number: 1,
          max_weight_lbs: 10000,
          position_from_front_in: 0,
          axle_type: 'STEER',
        },
      ],
      created_at: '2026-08-01T00:00:00Z',
      updated_at: '2026-08-01T00:00:00Z',
    },
  ];
}

let saved: { url: string; body: ProfileInput } | null = null;
let saveStatus = 200;
let saveBody: unknown = null;

function stubFleetApi(list: VehicleProfile[] = profiles()) {
  saved = null;
  saveStatus = 200;
  saveBody = null;
  const fetchMock = vi.fn((url: string, init: RequestInit = {}) => {
    if ((init.method ?? 'GET') === 'PUT') {
      saved = { url, body: JSON.parse(init.body as string) };
      return Promise.resolve(jsonResponse(saveBody ?? { ...list[0], ...saved.body }, saveStatus));
    }
    return Promise.resolve(jsonResponse(list));
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

/** The number input inside the labelled field (`Bed length (in)` etc). */
function fieldInput(el: FleetProfiles, label: string): HTMLInputElement {
  const field = Array.from(el.querySelectorAll('label')).find((l) =>
    text(l).startsWith(label),
  );
  if (!field) throw new Error(`no field labelled "${label}"`);
  return field.querySelector('input') as HTMLInputElement;
}

function axleRows(el: FleetProfiles): HTMLTableRowElement[] {
  return Array.from(el.querySelectorAll('tbody tr'));
}

beforeEach(() => {
  window.history.replaceState({}, '', '/fleet');
});

describe('FleetProfiles — listing', () => {
  it('summarises each vehicle by axle count and GVWR', async () => {
    stubFleetApi();
    const el = await mount<FleetProfiles>('ailm-fleet-profiles');

    const body = text(el);
    expect(body).toContain(`Freightliner M2 Flatbed 2 axles · ${grouped(33000)} lb`);
    expect(body).toContain(`International Box Truck 1 axles · ${grouped(26000)} lb`);
  });

  it('tells the operator how to bootstrap an empty fleet', async () => {
    stubFleetApi([]);
    const el = await mount<FleetProfiles>('ailm-fleet-profiles');

    expect(text(el)).toContain('No profiles. Run make seed.');
    expect(text(el)).toContain('Select a vehicle to edit its profile.');
  });

  it('surfaces a load failure instead of rendering an empty fleet', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse({ error: { code: 'internal', message: 'database unavailable' } }, 500),
      ),
    );
    const el = await mount<FleetProfiles>('ailm-fleet-profiles');

    expect(text(el)).toContain('database unavailable');
  });
});

describe('FleetProfiles — editing', () => {
  it('opens the first vehicle with its bed envelope and axle table populated', async () => {
    stubFleetApi();
    const el = await mount<FleetProfiles>('ailm-fleet-profiles');

    expect(fieldInput(el, 'Bed length').value).toBe('288');
    expect(fieldInput(el, 'Bed width').value).toBe('96');
    expect(fieldInput(el, 'GVWR').value).toBe('33000');
    expect(fieldInput(el, 'Tare weight').value).toBe('14000');

    const rows = axleRows(el);
    expect(rows).toHaveLength(2);
    const steer = rows[0].querySelectorAll('input');
    expect(steer[0].value).toBe('12000'); // rating
    expect(steer[1].value).toBe('0'); // position from front
    expect((rows[0].querySelector('select') as HTMLSelectElement).value).toBe('STEER');
  });

  it('swaps the draft when another vehicle is selected', async () => {
    stubFleetApi();
    const el = await mount<FleetProfiles>('ailm-fleet-profiles');

    await clickByText(el, 'button', 'International Box Truck');

    expect(fieldInput(el, 'GVWR').value).toBe('26000');
    expect(axleRows(el)).toHaveLength(1);
  });

  it('sends the edited axle rating to the vehicle-scoped upsert', async () => {
    stubFleetApi();
    const el = await mount<FleetProfiles>('ailm-fleet-profiles');

    const driveRating = axleRows(el)[1].querySelectorAll('input')[0];
    await setValue(el, driveRating, '23000');
    await clickByText(el, 'button', 'Save Profile');

    expect(saved!.url).toBe('/api/v1/fleet/profiles/11111111-1111-1111-1111-111111111111');
    expect(saved!.body.axles[1]).toEqual({
      axle_number: 2,
      max_weight_lbs: 23000,
      position_from_front_in: 240,
      axle_type: 'DRIVE',
    });
    // Untouched fields ride along — the endpoint is a whole-profile replace.
    expect(saved!.body.gvwr_lbs).toBe(33000);
    expect(saved!.body.name).toBe('Freightliner M2 Flatbed');
    expect(text(el)).toContain('Saved');
  });

  it('appends a new axle with a sequential number and drive defaults', async () => {
    stubFleetApi();
    const el = await mount<FleetProfiles>('ailm-fleet-profiles');

    await clickByText(el, 'button', 'Add axle');
    await clickByText(el, 'button', 'Save Profile');

    expect(axleRows(el)).toHaveLength(3);
    expect(saved!.body.axles).toHaveLength(3);
    expect(saved!.body.axles[2]).toEqual({
      axle_number: 3,
      max_weight_lbs: 20000,
      position_from_front_in: 200,
      axle_type: 'DRIVE',
    });
  });

  it('removes an axle from the draft', async () => {
    stubFleetApi();
    const el = await mount<FleetProfiles>('ailm-fleet-profiles');

    const remove = axleRows(el)[1].querySelector(
      'button[aria-label="Remove axle"]',
    ) as HTMLButtonElement;
    remove.click();
    await el.updateComplete;
    await clickByText(el, 'button', 'Save Profile');

    expect(saved!.body.axles).toHaveLength(1);
    expect(saved!.body.axles[0].axle_type).toBe('STEER');
  });

  it('reports a rejected save without clearing the operator’s edits', async () => {
    stubFleetApi();
    saveStatus = 422;
    saveBody = { error: { code: 'invalid', message: 'axle positions must increase' } };
    const el = await mount<FleetProfiles>('ailm-fleet-profiles');

    await setValue(el, fieldInput(el, 'Bed length'), '300');
    await clickByText(el, 'button', 'Save Profile');

    expect(text(el)).toContain('axle positions must increase');
    expect(text(el)).not.toContain('Saved');
    expect(fieldInput(el, 'Bed length').value).toBe('300');
  });

  // `Number('')` is 0, and the solver treats 0 as "unrated": load/solver.go
  // reports UNKNOWN (never a confident PASS) for a blank GVWR or axle rating,
  // and the whole-profile upsert would persist that 0. So a cleared box must
  // never be sent as a rating of ZERO — _setField/_setAxle map '' to undefined
  // (as the sibling CompliancePoints._set does) and the save is refused until
  // the operator supplies a real number.
  it('does not save a zero rating when a field is cleared', async () => {
    stubFleetApi();
    const el = await mount<FleetProfiles>('ailm-fleet-profiles');

    await setValue(el, fieldInput(el, 'GVWR'), '');
    await clickByText(el, 'button', 'Save Profile');

    expect(saved?.body.gvwr_lbs).not.toBe(0);
  });
});
