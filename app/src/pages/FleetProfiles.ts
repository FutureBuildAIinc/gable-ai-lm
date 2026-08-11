// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import { LitElement, html, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { icon } from '../lib/icons.ts';
import { Truck, Save, Plus, Trash2 } from 'lucide';
import {
  aiLmService,
  type VehicleProfile,
  type ProfileInput,
  type Axle,
} from '../services/aiLmService.ts';

@customElement('ailm-fleet-profiles')
export class FleetProfiles extends LitElement {
  createRenderRoot() { return this; }

  @state() private _profiles: VehicleProfile[] = [];
  @state() private _selectedId = '';
  @state() private _draft: ProfileInput | null = null;
  @state() private _error = '';
  @state() private _saving = false;
  @state() private _saved = false;

  connectedCallback() {
    super.connectedCallback();
    this._load();
  }

  private async _load() {
    try {
      this._profiles = await aiLmService.listProfiles();
      if (this._profiles.length > 0 && !this._selectedId) {
        this._select(this._profiles[0]);
      }
    } catch (err) {
      this._error = err instanceof Error ? err.message : String(err);
    }
  }

  private _select(p: VehicleProfile) {
    this._selectedId = p.gable_vehicle_id;
    this._saved = false;
    this._draft = {
      name: p.name,
      bed_length_in: p.bed_length_in,
      bed_width_in: p.bed_width_in,
      bed_height_in: p.bed_height_in,
      gvwr_lbs: p.gvwr_lbs,
      tare_weight_lbs: p.tare_weight_lbs,
      axles: p.axles.map((a) => ({
        axle_number: a.axle_number,
        max_weight_lbs: a.max_weight_lbs,
        position_from_front_in: a.position_from_front_in,
        axle_type: a.axle_type,
      })),
    };
  }

  private _setField(field: keyof ProfileInput, value: string) {
    if (!this._draft) return;
    const v = field === 'name' ? value : Number(value);
    this._draft = { ...this._draft, [field]: v } as ProfileInput;
  }

  private _setAxle(idx: number, field: keyof Omit<Axle, 'id'>, value: string) {
    if (!this._draft) return;
    const axles = [...this._draft.axles];
    const v = field === 'axle_type' ? value : Number(value);
    axles[idx] = { ...axles[idx], [field]: v };
    this._draft = { ...this._draft, axles };
  }

  private _addAxle() {
    if (!this._draft) return;
    const n = this._draft.axles.length + 1;
    this._draft = {
      ...this._draft,
      axles: [...this._draft.axles, { axle_number: n, max_weight_lbs: 20000, position_from_front_in: 200, axle_type: 'DRIVE' }],
    };
  }

  private _removeAxle(idx: number) {
    if (!this._draft) return;
    this._draft = { ...this._draft, axles: this._draft.axles.filter((_, i) => i !== idx) };
  }

  private async _save() {
    if (!this._draft || !this._selectedId) return;
    this._saving = true;
    this._error = '';
    this._saved = false;
    try {
      const updated = await aiLmService.upsertProfile(this._selectedId, this._draft);
      this._profiles = this._profiles.map((p) => (p.gable_vehicle_id === this._selectedId ? updated : p));
      this._saved = true;
    } catch (err) {
      this._error = err instanceof Error ? err.message : String(err);
    } finally {
      this._saving = false;
    }
  }

  private _numInput(label: string, field: keyof ProfileInput, value: number, unit: string) {
    return html`
      <label class="flex flex-col gap-1 text-xs text-zinc-400">
        ${label} <span class="text-zinc-600">(${unit})</span>
        <input
          type="number"
          .value=${String(value)}
          @change=${(e: Event) => this._setField(field, (e.target as HTMLInputElement).value)}
          class="bg-deep-space border border-white/10 rounded-lg px-3 py-2 text-sm font-mono text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50"
        />
      </label>
    `;
  }

  render() {
    return html`
      <div class="space-y-6 max-w-[1400px]">
        <header>
          <h1 class="text-2xl font-semibold flex items-center gap-2">${icon(Truck, 24, 'text-gable-green')} Fleet Profiles</h1>
          <p class="text-sm text-zinc-400 mt-1">Axle layout, bed dimensions, GVWR and tare weight per GableLBM vehicle.</p>
        </header>

        ${this._error
          ? html`<div class="px-4 py-2.5 rounded-lg border border-safety-red/30 bg-safety-red/10 text-safety-red text-sm">${this._error}</div>`
          : nothing}

        <div class="grid grid-cols-1 lg:grid-cols-4 gap-6">
          <div class="lg:col-span-1 space-y-1">
            ${this._profiles.length === 0
              ? html`<p class="text-sm text-zinc-500">No profiles. Run <span class="font-mono">make seed</span>.</p>`
              : this._profiles.map(
                  (p) => html`
                    <button
                      @click=${() => this._select(p)}
                      class="w-full text-left px-3 py-2.5 rounded-lg text-sm transition-all ${p.gable_vehicle_id === this._selectedId
                        ? 'bg-gable-green/10 text-gable-green border border-gable-green/30'
                        : 'text-zinc-300 hover:bg-white/5 border border-transparent'}"
                    >
                      <div class="font-medium">${p.name}</div>
                      <div class="text-xs text-zinc-500 font-mono">${p.axles.length} axles · ${p.gvwr_lbs.toLocaleString()} lb</div>
                    </button>
                  `,
                )}
          </div>

          <div class="lg:col-span-3">
            ${this._draft
              ? html`
                  <div class="glass-card rounded-xl p-5 space-y-5">
                    <label class="flex flex-col gap-1 text-xs text-zinc-400">
                      Vehicle name
                      <input
                        type="text"
                        .value=${this._draft.name}
                        @change=${(e: Event) => this._setField('name', (e.target as HTMLInputElement).value)}
                        class="bg-deep-space border border-white/10 rounded-lg px-3 py-2 text-sm text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50"
                      />
                    </label>

                    <div class="grid grid-cols-2 md:grid-cols-3 gap-3">
                      ${this._numInput('Bed length', 'bed_length_in', this._draft.bed_length_in, 'in')}
                      ${this._numInput('Bed width', 'bed_width_in', this._draft.bed_width_in, 'in')}
                      ${this._numInput('Bed height', 'bed_height_in', this._draft.bed_height_in, 'in')}
                      ${this._numInput('GVWR', 'gvwr_lbs', this._draft.gvwr_lbs, 'lb')}
                      ${this._numInput('Tare weight', 'tare_weight_lbs', this._draft.tare_weight_lbs, 'lb')}
                    </div>

                    <div>
                      <div class="flex items-center justify-between mb-2">
                        <h2 class="text-sm font-semibold text-zinc-300">Axles</h2>
                        <button @click=${this._addAxle} class="flex items-center gap-1.5 text-xs text-gable-green hover:underline">
                          ${icon(Plus, 14)} Add axle
                        </button>
                      </div>
                      <table class="w-full text-sm">
                        <thead>
                          <tr class="text-xs text-zinc-500 border-b border-white/5">
                            <th class="text-left font-medium py-2 pr-2">#</th>
                            <th class="text-right font-medium py-2 px-2">Max wt (lb)</th>
                            <th class="text-right font-medium py-2 px-2">Pos from front (in)</th>
                            <th class="text-left font-medium py-2 px-2">Type</th>
                            <th></th>
                          </tr>
                        </thead>
                        <tbody>
                          ${this._draft.axles.map(
                            (a, idx) => html`
                              <tr class="border-b border-white/5">
                                <td class="py-1.5 pr-2 font-mono text-zinc-400">${a.axle_number}</td>
                                <td class="py-1.5 px-2">
                                  <input type="number" .value=${String(a.max_weight_lbs)}
                                    @change=${(e: Event) => this._setAxle(idx, 'max_weight_lbs', (e.target as HTMLInputElement).value)}
                                    class="bg-deep-space border border-white/10 rounded px-2 py-1 text-right font-mono w-24 text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50" />
                                </td>
                                <td class="py-1.5 px-2">
                                  <input type="number" .value=${String(a.position_from_front_in)}
                                    @change=${(e: Event) => this._setAxle(idx, 'position_from_front_in', (e.target as HTMLInputElement).value)}
                                    class="bg-deep-space border border-white/10 rounded px-2 py-1 text-right font-mono w-24 text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50" />
                                </td>
                                <td class="py-1.5 px-2">
                                  <select .value=${a.axle_type}
                                    @change=${(e: Event) => this._setAxle(idx, 'axle_type', (e.target as HTMLSelectElement).value)}
                                    class="bg-deep-space border border-white/10 rounded px-2 py-1 text-white focus:outline-none focus:ring-1 focus:ring-gable-green/50">
                                    ${['STEER', 'DRIVE', 'TRAILER', 'TAG'].map((t) => html`<option value=${t} ?selected=${t === a.axle_type}>${t}</option>`)}
                                  </select>
                                </td>
                                <td class="py-1.5 text-right">
                                  <button @click=${() => this._removeAxle(idx)} class="text-zinc-500 hover:text-safety-red" aria-label="Remove axle">
                                    ${icon(Trash2, 16)}
                                  </button>
                                </td>
                              </tr>
                            `,
                          )}
                        </tbody>
                      </table>
                    </div>

                    <div class="flex items-center gap-3">
                      <button @click=${this._save} ?disabled=${this._saving}
                        class="flex items-center gap-2 bg-gable-green text-deep-space font-semibold px-4 py-2 rounded-lg hover:shadow-glow transition-all disabled:opacity-50">
                        ${icon(Save, 18)} ${this._saving ? 'Saving…' : 'Save Profile'}
                      </button>
                      ${this._saved ? html`<span class="text-sm text-gable-green">Saved ✓</span>` : nothing}
                    </div>
                  </div>
                `
              : html`<p class="text-sm text-zinc-500">Select a vehicle to edit its profile.</p>`}
          </div>
        </div>
      </div>
    `;
  }
}
