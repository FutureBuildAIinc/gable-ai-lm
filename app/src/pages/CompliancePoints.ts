// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import { LitElement, html, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { icon } from '../lib/icons.ts';
import { ShieldAlert, Plus } from 'lucide';
import {
  aiLmService,
  type RestrictedPoint,
  type RestrictedPointInput,
} from '../services/aiLmService.ts';

const TYPES = ['WEIGHT', 'HEIGHT', 'WIDTH', 'SEASONAL'] as const;

function blankDraft(): RestrictedPointInput {
  return {
    name: '',
    lat: 49.88,
    lng: -119.49,
    restriction_type: 'WEIGHT',
    notes: '',
  };
}

@customElement('ailm-compliance-points')
export class CompliancePoints extends LitElement {
  createRenderRoot() { return this; }

  @state() private _points: RestrictedPoint[] = [];
  @state() private _draft: RestrictedPointInput | null = null;
  @state() private _error = '';
  @state() private _saving = false;

  connectedCallback() {
    super.connectedCallback();
    this._load();
  }

  private async _load() {
    try {
      this._points = await aiLmService.listRestrictedPoints();
    } catch (err) {
      this._error = err instanceof Error ? err.message : String(err);
    }
  }

  private _set(field: keyof RestrictedPointInput, value: string) {
    if (!this._draft) return;
    let v: string | number = value;
    if (field === 'lat' || field === 'lng' || field === 'max_gross_weight_lbs' || field === 'max_axle_weight_lbs' || field === 'max_height_in') {
      v = value === '' ? (undefined as unknown as number) : Number(value);
    }
    this._draft = { ...this._draft, [field]: v } as RestrictedPointInput;
  }

  private async _save() {
    if (!this._draft) return;
    this._saving = true;
    this._error = '';
    try {
      const created = await aiLmService.createRestrictedPoint(this._draft);
      this._points = [...this._points, created];
      this._draft = null;
    } catch (err) {
      this._error = err instanceof Error ? err.message : String(err);
    } finally {
      this._saving = false;
    }
  }

  private _limitText(p: RestrictedPoint): string {
    if (p.max_gross_weight_lbs) return `${p.max_gross_weight_lbs.toLocaleString()} lb gross`;
    if (p.max_axle_weight_lbs) return `${p.max_axle_weight_lbs.toLocaleString()} lb/axle`;
    if (p.max_height_in) return `${p.max_height_in}" clearance`;
    return '—';
  }

  private _typeBadge(t: string) {
    const cls = {
      WEIGHT: 'bg-blueprint-blue/15 text-blueprint-blue',
      HEIGHT: 'bg-amber-warn/15 text-amber-warn',
      WIDTH: 'bg-purple-400/15 text-purple-300',
      SEASONAL: 'bg-gable-green/15 text-gable-green',
    }[t] || 'bg-white/10 text-zinc-300';
    return html`<span class="px-2 py-0.5 rounded-full text-xs font-medium ${cls}">${t}</span>`;
  }

  render() {
    return html`
      <div class="space-y-6 max-w-[1200px]">
        <header class="flex items-center justify-between">
          <div>
            <h1 class="text-2xl font-semibold flex items-center gap-2">${icon(ShieldAlert, 24, 'text-gable-green')} Restricted Points</h1>
            <p class="text-sm text-zinc-400 mt-1">Weight/height/width-limited bridges and overpasses flagged during routing.</p>
          </div>
          <button @click=${() => { this._draft = blankDraft(); }}
            class="flex items-center gap-2 bg-gable-green text-deep-space font-semibold px-4 py-2 rounded-lg hover:shadow-glow transition-all">
            ${icon(Plus, 18)} Add Point
          </button>
        </header>

        ${this._error
          ? html`<div class="px-4 py-2.5 rounded-lg border border-safety-red/30 bg-safety-red/10 text-safety-red text-sm">${this._error}</div>`
          : nothing}

        ${this._draft ? this._renderForm() : nothing}

        <div class="glass-card rounded-xl overflow-hidden">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-xs text-zinc-500 border-b border-white/5 bg-white/5">
                <th class="text-left font-medium py-3 px-4">Name</th>
                <th class="text-left font-medium py-3 px-4">Type</th>
                <th class="text-right font-medium py-3 px-4">Limit</th>
                <th class="text-right font-medium py-3 px-4">Lat / Lng</th>
                <th class="text-left font-medium py-3 px-4">Notes</th>
              </tr>
            </thead>
            <tbody>
              ${this._points.length === 0
                ? html`<tr><td colspan="5" class="py-6 text-center text-zinc-500">No restricted points. Run <span class="font-mono">make seed</span> or add one.</td></tr>`
                : this._points.map(
                    (p) => html`
                      <tr class="border-b border-white/5 hover:bg-white/[0.02]">
                        <td class="py-3 px-4 font-medium text-zinc-200">${p.name}</td>
                        <td class="py-3 px-4">${this._typeBadge(p.restriction_type)}</td>
                        <td class="py-3 px-4 text-right font-mono text-zinc-300">${this._limitText(p)}</td>
                        <td class="py-3 px-4 text-right font-mono text-xs text-zinc-500">${p.lat.toFixed(4)}, ${p.lng.toFixed(4)}</td>
                        <td class="py-3 px-4 text-zinc-400 text-xs">${p.notes}</td>
                      </tr>
                    `,
                  )}
            </tbody>
          </table>
        </div>
      </div>
    `;
  }

  private _renderForm() {
    const d = this._draft!;
    return html`
      <div class="glass-card rounded-xl p-5 space-y-4">
        <h2 class="text-sm font-semibold text-zinc-300">New Restricted Point</h2>
        <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
          <label class="flex flex-col gap-1 text-xs text-zinc-400">Name
            <input type="text" .value=${d.name} @change=${(e: Event) => this._set('name', (e.target as HTMLInputElement).value)}
              class="bg-deep-space border border-white/10 rounded-lg px-3 py-2 text-sm text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50" />
          </label>
          <label class="flex flex-col gap-1 text-xs text-zinc-400">Restriction type
            <select .value=${d.restriction_type} @change=${(e: Event) => this._set('restriction_type', (e.target as HTMLSelectElement).value)}
              class="bg-deep-space border border-white/10 rounded-lg px-3 py-2 text-sm text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50">
              ${TYPES.map((t) => html`<option value=${t} ?selected=${t === d.restriction_type}>${t}</option>`)}
            </select>
          </label>
          <label class="flex flex-col gap-1 text-xs text-zinc-400">Latitude
            <input type="number" step="0.0001" .value=${String(d.lat)} @change=${(e: Event) => this._set('lat', (e.target as HTMLInputElement).value)}
              class="bg-deep-space border border-white/10 rounded-lg px-3 py-2 text-sm font-mono text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50" />
          </label>
          <label class="flex flex-col gap-1 text-xs text-zinc-400">Longitude
            <input type="number" step="0.0001" .value=${String(d.lng)} @change=${(e: Event) => this._set('lng', (e.target as HTMLInputElement).value)}
              class="bg-deep-space border border-white/10 rounded-lg px-3 py-2 text-sm font-mono text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50" />
          </label>
          <label class="flex flex-col gap-1 text-xs text-zinc-400">Max gross weight (lb)
            <input type="number" .value=${d.max_gross_weight_lbs ? String(d.max_gross_weight_lbs) : ''} @change=${(e: Event) => this._set('max_gross_weight_lbs', (e.target as HTMLInputElement).value)}
              class="bg-deep-space border border-white/10 rounded-lg px-3 py-2 text-sm font-mono text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50" />
          </label>
          <label class="flex flex-col gap-1 text-xs text-zinc-400">Max height (in)
            <input type="number" .value=${d.max_height_in ? String(d.max_height_in) : ''} @change=${(e: Event) => this._set('max_height_in', (e.target as HTMLInputElement).value)}
              class="bg-deep-space border border-white/10 rounded-lg px-3 py-2 text-sm font-mono text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50" />
          </label>
        </div>
        <label class="flex flex-col gap-1 text-xs text-zinc-400">Notes
          <input type="text" .value=${d.notes} @change=${(e: Event) => this._set('notes', (e.target as HTMLInputElement).value)}
            class="bg-deep-space border border-white/10 rounded-lg px-3 py-2 text-sm text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50" />
        </label>
        <div class="flex items-center gap-3">
          <button @click=${this._save} ?disabled=${this._saving || !d.name}
            class="bg-gable-green text-deep-space font-semibold px-4 py-2 rounded-lg hover:shadow-glow transition-all disabled:opacity-50">
            ${this._saving ? 'Saving…' : 'Create Point'}
          </button>
          <button @click=${() => { this._draft = null; }} class="text-sm text-zinc-400 hover:text-white">Cancel</button>
        </div>
      </div>
    `;
  }
}
