// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * The dispatcher's write paths through the guided workflow.
 *
 * Everything in this file changes server state: locking a run, queueing and
 * resolving a same-day add, overriding a variable-size SKU's dimensions,
 * attaching yard proof and signing a truck off. Each one is a button that has
 * to send the right request to the right URL with the right approver on it —
 * a sign-off recorded against the wrong person, or a re-pack that silently
 * drops the tolerance, is an audit failure rather than a cosmetic one.
 *
 * Assertions are on the request the component actually issued and on the DOM it
 * rendered afterwards, never on the fixture that was fed back to it.
 *
 * The 3D visualiser and the Leaflet map are stubbed: WebGL and real layout
 * boxes, neither of which jsdom has.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../components/load/Load3DVisualizer.ts', () => ({}));
vi.mock('../components/routing/RouteMap.ts', () => ({}));

import type { PlanWorkflow } from './PlanWorkflow.ts';
import './PlanWorkflow.ts';
import type { WorkflowPlan } from '../services/aiLmService.ts';
import { clickByText, flush, jsonResponse, mount, setValue, text } from '../test/dom.ts';

/** A packed, reviewed, single-truck day with one order on it. */
function plan(): WorkflowPlan {
  return {
    id: 'plan-1',
    plan_date: '2026-08-12',
    status: 'REVIEWED',
    depot_lat: 49.8863,
    depot_lng: -119.4666,
    created_at: '2026-08-11T14:00:00Z',
    updated_at: '2026-08-11T15:12:00Z',
    unassigned_orders: [],
    orders: [
      {
        order_id: 'o-1',
        customer_name: 'Ridgeview Framing',
        address: '2200 Benvoulin Rd',
        lat: 49.87,
        lng: -119.45,
        total_weight_lbs: 12_345,
        total_volume_cuft: 187,
        max_length_in: 240,
        piece_count: 24,
        shape_profile: 'LONG_LOAD',
        routable: true,
        priority: false,
        issues: [],
        lines: [
          {
            product_id: 'p-2',
            sku: 'WRC-2X8-20',
            name: 'Western red cedar 2x8x20',
            quantity: 24,
            unit_weight_lbs: 39.2,
            unit_length_in: 239.6,
            unit_width_in: 7.25,
            unit_height_in: 1.5,
            stackable: false,
            has_geometry: false,
            line_weight_lbs: 940.8,
            line_volume_cuft: 44,
          },
        ],
      },
    ],
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
          {
            order_id: 'o-2',
            sequence: 2,
            lat: 49.86,
            lng: -119.47,
            address: '910 Gordon Dr',
            customer_name: 'Okanagan Decks',
            weight_lbs: 4_820,
            priority: false,
          },
        ],
        load_plan: {
          id: 'lp-1',
          gable_vehicle_id: 'veh-1',
          total_weight_lbs: 28_400,
          balance_score: 0.9,
          gvw_status: 'PASS',
          unplaced: [],
          max_load_height_in: 44,
          created_at: '2026-08-11T15:00:00Z',
          axle_loads: [
            {
              axle_number: 1,
              weight_lbs: 9_600,
              max_weight_lbs: 12_000,
              utilization: 0.8,
              status: 'PASS',
            },
          ],
          placements: [
            {
              item_id: 'i-1',
              sku: 'WRC-2X8-20',
              x: 0,
              y: 0,
              z: 0,
              length_in: 240,
              width_in: 7.25,
              height_in: 1.5,
              weight_lbs: 39.2,
              axle_group: 1,
              step: 1,
            },
            {
              item_id: 'i-2',
              sku: 'WRC-2X8-20',
              x: 0,
              y: 7.25,
              z: 0,
              length_in: 240,
              width_in: 7.25,
              height_in: 1.5,
              weight_lbs: 39.2,
              axle_group: 1,
              step: 2,
            },
          ],
        },
        compliance: {
          status: 'PASS',
          flags: [],
          actions: [],
          checked_gross_lbs: 28_400,
          checked_max_axle_lbs: 9_600,
          checked_height_in: 44,
        },
        proof: { attachments: [], signed_off: false },
      },
    ],
  };
}

/** Every request the component issued, in order. */
interface Sent {
  url: string;
  method: string;
  body: Record<string, unknown> | undefined;
}

let sent: Sent[];

/**
 * Serves `plan()` (with `mutate` applied) for every request and records what was
 * asked for. Writes echo the same plan back, which is what the real API does.
 */
async function mountPlan(
  mutate: (p: WorkflowPlan) => void = () => {},
): Promise<PlanWorkflow> {
  const p = plan();
  mutate(p);
  sent = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init: RequestInit = {}) => {
      sent.push({
        url,
        method: init.method ?? 'GET',
        body: init.body ? JSON.parse(init.body as string) : undefined,
      });
      return Promise.resolve(jsonResponse(p));
    }),
  );
  return mount<PlanWorkflow>('ailm-plan-workflow');
}

/** The last write (anything that was not a plain GET). */
function lastWrite(): Sent {
  const writes = sent.filter((s) => s.method !== 'GET');
  if (writes.length === 0) throw new Error(`no write issued; saw ${JSON.stringify(sent)}`);
  return writes[writes.length - 1];
}

beforeEach(() => {
  window.history.replaceState({}, '', '/plan');
  localStorage.setItem('ailm_name', 'Dana R.');
});

describe('PlanWorkflow — lock windows (T2-3)', () => {
  it('reports an open run and offers both lock windows', async () => {
    const el = await mountPlan();
    const body = text(el);

    expect(body).toContain('Run open');
    expect(body).toContain('Lock morning');
    expect(body).toContain('Lock afternoon');
    expect(body).not.toContain('Unlock');
  });

  it('locks the morning window under the signed-in dispatcher', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Lock morning');

    expect(lastWrite()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/lock',
      method: 'POST',
      body: { locked: true, window: 'MORNING', locked_by: 'Dana R.' },
    });
  });

  it('distinguishes the afternoon window', async () => {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Lock afternoon');
    expect(lastWrite().body).toMatchObject({ window: 'AFTERNOON' });
  });

  it('attributes the lock to a placeholder when nobody is signed in', async () => {
    localStorage.removeItem('ailm_name');
    const el = await mountPlan();
    await clickByText(el, 'button', 'Lock morning');

    expect(lastWrite().body).toMatchObject({ locked_by: 'Yard staff' });
  });

  it('sends a reasoned unlock for a locked run', async () => {
    const el = await mountPlan((p) => {
      p.lock = { locked: true, window: 'MORNING', lock_at: '06:00' };
    });
    await clickByText(el, 'button', 'Unlock');

    expect(lastWrite()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/unlock',
      method: 'POST',
      body: { reason: 'manual unlock', locked_by: 'Dana R.' },
    });
  });
});

describe('PlanWorkflow — late same-day adds (T2-3)', () => {
  it('stays hidden on an open run with nothing pending', async () => {
    const el = await mountPlan();
    expect(text(el)).not.toContain('Late same-day add');
  });

  it('appears once the run is locked, even with nothing pending', async () => {
    const el = await mountPlan((p) => {
      p.lock = { locked: true };
    });
    expect(text(el)).toContain('Late same-day add');
  });

  it('keeps Queue add disabled until an order id is typed', async () => {
    const el = await mountPlan((p) => {
      p.lock = { locked: true };
    });

    const queue = Array.from(el.querySelectorAll('button')).find((b) =>
      text(b).includes('Queue add'),
    ) as HTMLButtonElement;
    expect(queue.disabled).toBe(true);

    const input = el.querySelector('input[placeholder="GableLBM order id"]') as HTMLInputElement;
    await setValue(el, input, 'o-9', 'input');

    expect(
      (
        Array.from(el.querySelectorAll('button')).find((b) =>
          text(b).includes('Queue add'),
        ) as HTMLButtonElement
      ).disabled,
    ).toBe(false);
  });

  it('queues the trimmed order id against the requester', async () => {
    const el = await mountPlan((p) => {
      p.lock = { locked: true };
    });

    const input = el.querySelector('input[placeholder="GableLBM order id"]') as HTMLInputElement;
    await setValue(el, input, '  o-9  ', 'input');
    await clickByText(el, 'button', 'Queue add');

    expect(lastWrite()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/late-adds',
      method: 'POST',
      body: { order_id: 'o-9', requested_by: 'Dana R.' },
    });
  });

  it('lists a pending add with approve and reject actions', async () => {
    const el = await mountPlan((p) => {
      p.late_adds = [
        {
          order_id: 'o-9',
          customer_name: 'Highstreet Homes',
          status: 'PENDING',
          requested_at: '2026-08-11T16:00:00Z',
        },
      ];
    });

    const body = text(el);
    expect(body).toContain('PENDING');
    expect(body).toContain('Highstreet Homes');
    expect(body).toContain('Approve');
    expect(body).toContain('Reject');
  });

  it('hides an already-resolved add', async () => {
    const el = await mountPlan((p) => {
      p.late_adds = [
        {
          order_id: 'o-9',
          customer_name: 'Highstreet Homes',
          status: 'APPROVED',
          requested_at: '2026-08-11T16:00:00Z',
        },
      ];
    });

    expect(text(el)).not.toContain('Highstreet Homes');
  });

  it('records the approver on an approval', async () => {
    const el = await mountPlan((p) => {
      p.late_adds = [
        { order_id: 'o-9', status: 'PENDING', requested_at: '2026-08-11T16:00:00Z' },
      ];
    });
    await clickByText(el, 'button', 'Approve');

    expect(lastWrite()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/late-adds/o-9/resolve',
      method: 'POST',
      body: { reject: false, approved_by: 'Dana R.' },
    });
  });

  it('records the approver on a rejection too', async () => {
    const el = await mountPlan((p) => {
      p.late_adds = [
        { order_id: 'o-9', status: 'PENDING', requested_at: '2026-08-11T16:00:00Z' },
      ];
    });
    await clickByText(el, 'button', 'Reject');

    expect(lastWrite().body).toEqual({ reject: true, approved_by: 'Dana R.' });
  });
});

describe('PlanWorkflow — dimension overrides for variable-size SKUs (T2-2)', () => {
  /** Open step 1 and reveal the inline dimension editor for the cedar line. */
  async function openEditor(el: PlanWorkflow) {
    await clickByText(el, 'button', 'Ingest & Analyze');
    const ruler = el.querySelector('button[aria-label="Set dimensions"]') as HTMLButtonElement;
    ruler.click();
    await el.updateComplete;
    await flush();
    await el.updateComplete;
  }

  function dimInputs(el: PlanWorkflow): HTMLInputElement[] {
    return Array.from(el.querySelectorAll('input[type="number"]'));
  }

  it('seeds the editor from the line geometry, rounded to whole inches', async () => {
    const el = await mountPlan();
    await openEditor(el);

    // 239.6 in on the wire is a 240 in board once measured.
    expect(dimInputs(el).map((i) => i.value)).toEqual(['240', '7', '2', '0']);
  });

  it('seeds the editor from an existing override instead of the raw geometry', async () => {
    const el = await mountPlan((p) => {
      p.orders[0].lines[0].dim_override = {
        length_in: 244,
        width_in: 8,
        height_in: 2,
        tolerance_pct: 15,
        source: 'AVERAGE',
      };
    });
    await openEditor(el);

    expect(dimInputs(el).map((i) => i.value)).toEqual(['244', '8', '2', '15']);
  });

  it('PUTs the edited dimensions with the product it applies to', async () => {
    const el = await mountPlan();
    await openEditor(el);

    const [length, , , tolerance] = dimInputs(el);
    await setValue(el, length, '244', 'input');
    await setValue(el, tolerance, '15', 'input');
    await clickByText(el, 'button', 'Apply & re-pack');

    expect(lastWrite()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/orders/o-1/dimensions',
      method: 'PUT',
      body: {
        product_id: 'p-2',
        sku: 'WRC-2X8-20',
        length_in: 244,
        width_in: 7,
        height_in: 2,
        tolerance_pct: 15,
        source: 'MEASURED',
      },
    });
  });

  it('refuses to apply a zero dimension', async () => {
    // A zero-length article makes the packer's volume budget meaningless.
    const el = await mountPlan();
    await openEditor(el);

    await setValue(el, dimInputs(el)[0], '0', 'input');

    const apply = Array.from(el.querySelectorAll('button')).find((b) =>
      text(b).includes('Apply & re-pack'),
    ) as HTMLButtonElement;
    expect(apply.disabled).toBe(true);
  });

  it('closes the editor without writing anything on cancel', async () => {
    const el = await mountPlan();
    await openEditor(el);
    await clickByText(el, 'button', 'Cancel');

    expect(el.querySelectorAll('input[type="number"]')).toHaveLength(0);
    expect(sent.filter((s) => s.method !== 'GET')).toEqual([]);
  });
});

describe('PlanWorkflow — yard proof and sign-off (T1-6)', () => {
  async function packStep(mutate: (p: WorkflowPlan) => void = () => {}): Promise<PlanWorkflow> {
    const el = await mountPlan(mutate);
    await clickByText(el, 'button', 'Pack Loads');
    return el;
  }

  function proofCard(el: PlanWorkflow): Element {
    const card = Array.from(el.querySelectorAll('.glass-card')).find((c) =>
      text(c).startsWith('Yard proof'),
    );
    if (!card) throw new Error('no Yard proof card rendered');
    return card;
  }

  it('reports a truck with no proof as PENDING', async () => {
    const el = await packStep();
    expect(text(proofCard(el))).toContain('PENDING');
  });

  it('will not let a truck be signed off before any proof exists', async () => {
    const el = await packStep();
    const signOff = Array.from(proofCard(el).querySelectorAll('button')).find((b) =>
      text(b).includes('Sign off'),
    ) as HTMLButtonElement;

    expect(signOff.disabled).toBe(true);
    expect(signOff.title).toBe('Attach at least one photo/video first');
  });

  it('attaches a photo against the truck and the person who took it', async () => {
    const el = await packStep();
    const url = proofCard(el).querySelector('input[type="text"]') as HTMLInputElement;
    await setValue(el, url, ' https://cdn.example.test/load.jpg ', 'input');
    await clickByText(el, 'button', 'Attach');

    expect(lastWrite()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/loads/veh-1/proof',
      method: 'POST',
      body: {
        url: 'https://cdn.example.test/load.jpg',
        kind: 'PHOTO',
        added_by: 'Dana R.',
      },
    });
  });

  it('sends the chosen attachment kind', async () => {
    const el = await packStep();
    const kind = proofCard(el).querySelector('select') as HTMLSelectElement;
    await setValue(el, kind, 'VIDEO');
    const url = proofCard(el).querySelector('input[type="text"]') as HTMLInputElement;
    await setValue(el, url, 'https://cdn.example.test/load.mp4', 'input');
    await clickByText(el, 'button', 'Attach');

    expect(lastWrite().body).toMatchObject({ kind: 'VIDEO' });
  });

  it('renders an attached photo as a link to the full-size asset', async () => {
    const el = await packStep((p) => {
      p.loads[0].proof = {
        attachments: [
          {
            url: 'https://cdn.example.test/load.jpg',
            kind: 'PHOTO',
            caption: 'rear quarter',
            added_at: '2026-08-11T15:10:00Z',
          },
        ],
        signed_off: false,
      };
    });

    const link = proofCard(el).querySelector('a') as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe('https://cdn.example.test/load.jpg');
    expect(link.getAttribute('rel')).toBe('noopener');
    expect(link.querySelector('img')?.getAttribute('alt')).toBe('rear quarter');
  });

  it('signs a load off under the signed-in name once proof exists', async () => {
    const el = await packStep((p) => {
      p.loads[0].proof = {
        attachments: [
          { url: 'https://cdn.example.test/load.jpg', kind: 'PHOTO', added_at: 'x' },
        ],
        signed_off: false,
      };
    });
    await clickByText(el, 'button', 'Sign off');

    expect(lastWrite()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/loads/veh-1/sign-off',
      method: 'POST',
      body: { signed_by: 'Dana R.' },
    });
  });

  it('names the signer once a load is signed off', async () => {
    const el = await packStep((p) => {
      p.loads[0].proof = {
        attachments: [
          { url: 'https://cdn.example.test/load.jpg', kind: 'PHOTO', added_at: 'x' },
        ],
        signed_off: true,
        signed_by: 'Marc T.',
      };
    });

    const body = text(proofCard(el));
    expect(body).toContain('READY');
    expect(body).toContain('Signed off by Marc T.');
  });
});

describe('PlanWorkflow — stop resequencing', () => {
  async function packStep(): Promise<PlanWorkflow> {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Pack Loads');
    return el;
  }

  it('explains that stop 1 is delivered first and therefore packed last', async () => {
    const el = await packStep();
    expect(text(el)).toContain('Stop 1 delivers first → packed last (rear of bed)');
  });

  it('sends the swapped order when a stop is moved later', async () => {
    const el = await packStep();
    const down = el.querySelectorAll('button[aria-label="Move stop later"]')[0] as HTMLButtonElement;
    down.click();
    await el.updateComplete;
    await flush();

    expect(lastWrite()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/loads/veh-1/sequence',
      method: 'PUT',
      body: { order_ids: ['o-2', 'o-1'], override: false, approved_by: '' },
    });
  });

  it('cannot move the first stop earlier or the last stop later', async () => {
    const el = await packStep();
    const up = Array.from(
      el.querySelectorAll('button[aria-label="Move stop earlier"]'),
    ) as HTMLButtonElement[];
    const down = Array.from(
      el.querySelectorAll('button[aria-label="Move stop later"]'),
    ) as HTMLButtonElement[];

    expect(up[0].disabled).toBe(true);
    expect(down[down.length - 1].disabled).toBe(true);
  });

  it('marks a stop priority so it is delivered first', async () => {
    const el = await packStep();
    const star = el.querySelectorAll(
      'button[aria-label="Toggle priority delivery"]',
    )[1] as HTMLButtonElement;
    star.click();
    await el.updateComplete;
    await flush();

    expect(lastWrite()).toEqual({
      url: '/api/v1/workflow/plans/plan-1/stops/o-2/priority',
      method: 'PUT',
      body: { priority: true, override: false, approved_by: '' },
    });
  });

  it('clears an existing priority rather than re-setting it', async () => {
    const el = await mountPlan((p) => {
      p.loads[0].stops[0].priority = true;
    });
    await clickByText(el, 'button', 'Pack Loads');

    const star = el.querySelector(
      'button[aria-label="Toggle priority delivery"]',
    ) as HTMLButtonElement;
    star.click();
    await el.updateComplete;
    await flush();

    expect(lastWrite().body).toMatchObject({ priority: false });
  });
});

describe('PlanWorkflow — AI dispatch briefing', () => {
  it('does not call the model until the panel is opened', async () => {
    const el = await mountPlan();
    expect(text(el)).toContain('AI dispatch briefing');
    expect(sent.some((s) => s.url.endsWith('/briefing'))).toBe(false);
  });

  it('fetches and renders the briefing when opened', async () => {
    const p = plan();
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) =>
        Promise.resolve(
          url.endsWith('/briefing')
            ? jsonResponse({
                available: true,
                model: 'claude-opus-5',
                text: 'Two trucks, both within GVW. Watch the Bennett Bridge seasonal limit.',
              })
            : jsonResponse(p),
        ),
      ),
    );
    const el = await mount<PlanWorkflow>('ailm-plan-workflow');
    await clickByText(el, 'button', 'AI dispatch briefing');

    const body = text(el);
    expect(body).toContain('Watch the Bennett Bridge seasonal limit.');
    expect(body).toContain('claude-opus-5');
    expect(body).toContain('Regenerate');
  });

  it('explains how to enable AI instead of pretending it produced a briefing', async () => {
    const p = plan();
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) =>
        Promise.resolve(
          url.endsWith('/briefing')
            ? jsonResponse({
                available: false,
                message: 'No AI key configured — set one in Tech Admin > AI Settings.',
              })
            : jsonResponse(p),
        ),
      ),
    );
    const el = await mount<PlanWorkflow>('ailm-plan-workflow');
    await clickByText(el, 'button', 'AI dispatch briefing');

    expect(text(el)).toContain('No AI key configured');
  });

  it('surfaces a briefing request failure in the panel, not as a blank body', async () => {
    const p = plan();
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) =>
        Promise.resolve(
          url.endsWith('/briefing')
            ? jsonResponse({ error: { code: 'upstream', message: 'model timed out' } }, 504)
            : jsonResponse(p),
        ),
      ),
    );
    const el = await mount<PlanWorkflow>('ailm-plan-workflow');
    await clickByText(el, 'button', 'AI dispatch briefing');

    expect(text(el)).toContain('model timed out');
  });
});

describe('PlanWorkflow — packing playback', () => {
  async function packStep(): Promise<PlanWorkflow> {
    const el = await mountPlan();
    await clickByText(el, 'button', 'Pack Loads');
    return el;
  }

  function slider(el: PlanWorkflow): HTMLInputElement {
    return el.querySelector('input[type="range"]') as HTMLInputElement;
  }

  it('spans every placed piece and starts showing the full load', async () => {
    const el = await packStep();
    expect(slider(el).max).toBe('2');
    expect(text(el)).toContain('2 / 2 pcs');
  });

  it('scrubs to an intermediate packing step', async () => {
    const el = await packStep();
    await setValue(el, slider(el), '1', 'input');
    expect(text(el)).toContain('1 / 2 pcs');
  });

  it('treats scrubbing to the end as "show the whole load"', async () => {
    const el = await packStep();
    await setValue(el, slider(el), '2', 'input');
    expect(text(el)).toContain('2 / 2 pcs');
  });

  it('resets to the full load from any partial step', async () => {
    const el = await packStep();
    await setValue(el, slider(el), '1', 'input');

    const reset = el.querySelector('button[title="Show full load"]') as HTMLButtonElement;
    reset.click();
    await el.updateComplete;

    expect(text(el)).toContain('2 / 2 pcs');
  });

  it('offers no playback controls for a truck with nothing placed', async () => {
    const el = await mountPlan((p) => {
      p.loads[0].load_plan!.placements = [];
    });
    await clickByText(el, 'button', 'Pack Loads');

    expect(el.querySelector('input[type="range"]')).toBeNull();
  });
});
