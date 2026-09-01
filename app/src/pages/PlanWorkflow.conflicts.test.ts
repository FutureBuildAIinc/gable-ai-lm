// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * What a dispatcher is told when a write comes back 409.
 *
 * The backend answers 409 for two different refusals that need OPPOSITE things
 * from the person at the screen:
 *
 *   - a VERSION CONFLICT — somebody else edited this plan between the read and
 *     the write. The plan on screen is stale and the recovery is to reload.
 *   - a BUSY DATE (`error.code: "DATE_BUSY"`) — another dispatcher is already
 *     pushing, re-planning or re-assigning that DAY. Nothing was gated, sent to
 *     GableLBM or written; the plan on screen is still correct and the recovery
 *     is simply to repeat.
 *
 * This page used to render one hardcoded banner for every 409 — "Someone else
 * changed this plan while you were working. Your change was not applied. Reload
 * to see theirs, then redo yours." — and threw the server's sentence away. For a
 * busy date every clause of that was false: nobody changed the plan, there was
 * nothing to reload past, and the day that was actually shut was never named.
 * The backend's comment claimed the two were "told apart by the sentence"; the
 * app never showed the sentence.
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
import { clickByText, jsonResponse, mount, text } from '../test/dom.ts';

/** The sentence the Go handler actually sends for a held date. */
const DATE_BUSY_SENTENCE =
  '2026-08-12 is already being pushed by someone else, so this push did not run. ' +
  'Nothing was sent to GableLBM and nothing on the dispatch board changed — wait a moment and try again.';

/** The hardcoded banner that used to answer BOTH 409s. */
const STALE_PLAN_BANNER = 'Someone else changed this plan while you were working';

/** A reviewed, packed, signed single-truck day — one press away from a push. */
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
        lines: [],
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
        stops: [
          {
            order_id: 'o-1',
            sequence: 1,
            lat: 49.87,
            lng: -119.45,
            customer_name: 'Ridgeview Framing',
            weight_lbs: 12_345,
            priority: false,
          },
        ],
        load_plan: {
          id: 'lp-1',
          gable_vehicle_id: 'veh-1',
          total_weight_lbs: 12_345,
          balance_score: 0.9,
          gvw_status: 'PASS',
          unplaced: [],
          max_load_height_in: 44,
          created_at: '2026-08-11T15:00:00Z',
          axle_loads: [
            { axle_number: 1, weight_lbs: 9_600, max_weight_lbs: 12_000, utilization: 0.8, status: 'PASS' },
          ],
          placements: [],
        },
        compliance: {
          status: 'PASS',
          flags: [],
          actions: [],
          checked_gross_lbs: 12_345,
          checked_max_axle_lbs: 9_600,
          checked_height_in: 44,
        },
        proof: {
          attachments: [
            { url: 'https://yard.example/load-1.jpg', kind: 'PHOTO', added_at: '2026-08-11T15:05:00Z' },
          ],
          signed_off: true,
          signed_by: 'Dana R.',
          signed_at: '2026-08-11T15:06:00Z',
        },
      },
    ],
  };
}

interface Sent {
  url: string;
  method: string;
  body: Record<string, unknown> | undefined;
}

/** The JSON a recorded request carried. */
function bodyOf(s: Sent): Record<string, unknown> {
  if (!s.body) throw new Error(`request to ${s.url} carried no body`);
  return s.body;
}

let sent: Sent[];

/** The standard Go error envelope, as pkg/httputil writes it. */
function errorEnvelope(code: string, message: string, status: number): Response {
  return jsonResponse({ error: { code, message }, meta: { request_id: 'req-1' } }, status);
}

/**
 * Mounts the page with a fetch double that serves the plan for reads and hands
 * each write the next queued response (falling back to "the write succeeded").
 */
async function mountWith(writes: Response[]): Promise<PlanWorkflow> {
  const p = plan();
  const queue = [...writes];
  sent = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string, init: RequestInit = {}) => {
      const method = init.method ?? 'GET';
      sent.push({ url, method, body: init.body ? JSON.parse(init.body as string) : undefined });
      if (method === 'GET') return Promise.resolve(jsonResponse(p));
      const next = queue.shift();
      return Promise.resolve(next ?? jsonResponse({ ...p, status: 'PUSHED' }));
    }),
  );
  return mount<PlanWorkflow>('ailm-plan-workflow');
}

const pushes = () => sent.filter((s) => s.url.endsWith('/push')).length;

beforeEach(() => {
  window.history.replaceState({}, '', '/plan');
  localStorage.setItem('ailm_name', 'Dana R.');
});

describe('PlanWorkflow — a dispatch date somebody else is holding', () => {
  it("shows the server's sentence, which names the day that is shut", async () => {
    const el = await mountWith([errorEnvelope('DATE_BUSY', DATE_BUSY_SENTENCE, 409)]);
    await clickByText(el, 'button', 'Push to GableLBM dispatch');

    const body = text(el);
    expect(body).toContain(DATE_BUSY_SENTENCE);
    // The date is the whole point: a dispatcher with three days open cannot
    // act on "try again" without being told which one is busy.
    expect(body).toContain('2026-08-12');
  });

  it('does not tell the dispatcher their plan was changed underneath them', async () => {
    const el = await mountWith([errorEnvelope('DATE_BUSY', DATE_BUSY_SENTENCE, 409)]);
    await clickByText(el, 'button', 'Push to GableLBM dispatch');

    const body = text(el);
    // Nobody changed the plan, so this sentence is false...
    expect(body).not.toContain(STALE_PLAN_BANNER);
    // ...and reloading is not the fix, so it must not be the button offered.
    expect(body).not.toContain('Reload plan');
    expect(body).toContain('Try again');
  });

  it('repeats the refused push when the dispatcher presses Try again', async () => {
    const el = await mountWith([errorEnvelope('DATE_BUSY', DATE_BUSY_SENTENCE, 409)]);
    await clickByText(el, 'button', 'Push to GableLBM dispatch');
    expect(pushes()).toBe(1);

    await clickByText(el, 'button', 'Try again');

    // A plain repeat of the SAME action — not a reload, and not a different
    // request. That is only safe because the refusal promised nothing was sent.
    expect(pushes()).toBe(2);
    expect(sent[sent.length - 1]).toMatchObject({
      url: '/api/v1/workflow/plans/plan-1/push',
      method: 'POST',
    });
  });

  it('clears the banner once the retry lands', async () => {
    const el = await mountWith([errorEnvelope('DATE_BUSY', DATE_BUSY_SENTENCE, 409)]);
    await clickByText(el, 'button', 'Push to GableLBM dispatch');
    expect(text(el)).toContain(DATE_BUSY_SENTENCE);

    await clickByText(el, 'button', 'Try again'); // the queue is empty, so this succeeds

    const body = text(el);
    expect(body).not.toContain(DATE_BUSY_SENTENCE);
    expect(body).not.toContain('Try again');
    expect(body).toContain('PUSHED');
  });

  it('reports a held date on a re-plan too, in that action’s own words', async () => {
    const replanSentence =
      '2026-08-12 is already being changed by someone else, so the day was not re-planned. ' +
      'Nothing was sent to GableLBM and nothing on the dispatch board changed — wait a moment and try again.';
    const el = await mountWith([errorEnvelope('DATE_BUSY', replanSentence, 409)]);

    await clickByText(el, 'button', 'Ingest & Analyze'); // the stepper, back to step 1
    await clickByText(el, 'button', 'Re-ingest orders');

    const body = text(el);
    expect(body).toContain(replanSentence);
    expect(body).not.toContain(STALE_PLAN_BANNER);
    // A busy date is not an approval prompt either: there is nobody to approve.
    expect(body).not.toContain('Approve & override');
  });
});

describe('PlanWorkflow — a held date during an approved override', () => {
  it('keeps the approval and offers the same approved action again', async () => {
    const lockedSentence =
      'a re-plan of 2026-08-12 would strand 2 live route(s) — requires manual approval (override)';
    const busySentence =
      '2026-08-12 is already being changed by someone else, so the day was not re-planned. ' +
      'Nothing was sent to GableLBM and nothing on the dispatch board changed — wait a moment and try again.';
    const el = await mountWith([
      errorEnvelope('LOCKED', lockedSentence, 423),
      errorEnvelope('DATE_BUSY', busySentence, 409),
    ]);

    await clickByText(el, 'button', 'Ingest & Analyze');
    await clickByText(el, 'button', 'Re-ingest orders');
    expect(text(el)).toContain('Approve & override');

    await clickByText(el, 'button', 'Approve & override');

    const body = text(el);
    // Nothing was applied and nobody withdrew the approval, so the dispatcher
    // must not be sent back through the approval conversation.
    expect(body).toContain(busySentence);
    expect(body).toContain('Approve & override');
    expect(body).toContain('Try again');
    expect(body).not.toContain(STALE_PLAN_BANNER);
  });

  it('repeats the OVERRIDDEN action, not the unapproved one', async () => {
    const el = await mountWith([
      errorEnvelope('LOCKED', 'requires manual approval (override)', 423),
      errorEnvelope(
        'DATE_BUSY',
        '2026-08-12 is already being changed by someone else, so the day was not re-planned. try again.',
        409,
      ),
    ]);
    await clickByText(el, 'button', 'Ingest & Analyze');
    await clickByText(el, 'button', 'Re-ingest orders');
    await clickByText(el, 'button', 'Approve & override');

    await clickByText(el, 'button', 'Try again');

    // Losing the override on the retry would silently re-ask a question the
    // dispatcher has already answered, and answer it "no".
    expect(bodyOf(sent[sent.length - 1])).toMatchObject({
      override: true,
      approved_by: 'Dana R.',
    });
  });
});

describe('PlanWorkflow — a plan somebody else edited', () => {
  it('still gets the reload banner, so the two 409s stay told apart', async () => {
    const el = await mountWith([
      errorEnvelope('CONFLICT', 'workflow plan was modified concurrently — reload and retry', 409),
    ]);
    await clickByText(el, 'button', 'Push to GableLBM dispatch');

    const body = text(el);
    expect(body).toContain(STALE_PLAN_BANNER);
    expect(body).toContain('Reload plan');
    // The busy-date affordance must NOT appear: repeating a write that lost an
    // optimistic-lock race re-applies this dispatcher's intent over somebody
    // else's change.
    expect(body).not.toContain('Try again');
  });

  it('reloads rather than repeating when the dispatcher presses Reload plan', async () => {
    const el = await mountWith([
      errorEnvelope('CONFLICT', 'workflow plan was modified concurrently — reload and retry', 409),
    ]);
    await clickByText(el, 'button', 'Push to GableLBM dispatch');
    expect(pushes()).toBe(1);

    await clickByText(el, 'button', 'Reload plan');

    expect(pushes()).toBe(1);
    expect(sent[sent.length - 1].method).toBe('GET');
  });
});
