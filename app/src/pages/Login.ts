// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import { LitElement, html, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { icon } from '../lib/icons.ts';
import { Truck, LogIn, Mail, AlertTriangle } from 'lucide';
import { aiLmService } from '../services/aiLmService.ts';
import { router } from '../lib/router.ts';

/**
 * <ailm-login> — standalone staff login (COMM-1 pillar 4). Email → POST
 * /api/v1/auth/login (validated against GableLBM). On success the AI_LM session
 * token is stored under the same localStorage key fetchClient reads, then the
 * dispatcher is sent to the Load Planner. Rendered with no app shell.
 */
@customElement('ailm-login')
export class Login extends LitElement {
  createRenderRoot() { return this; }

  @state() private _email = '';
  @state() private _busy = false;
  @state() private _error = '';

  private async _submit(e: Event) {
    e.preventDefault();
    const email = this._email.trim();
    if (!email || this._busy) return;
    this._busy = true;
    this._error = '';
    try {
      const res = await aiLmService.login(email);
      localStorage.setItem('token', res.token);
      if (res.name) localStorage.setItem('ailm_name', res.name);
      router.navigate('/plan');
    } catch (err) {
      this._error = err instanceof Error ? err.message : String(err);
    } finally {
      this._busy = false;
    }
  }

  render() {
    return html`
      <div class="min-h-screen bg-deep-space text-foreground flex items-center justify-center p-6 font-sans selection:bg-gable-green/30">
        <div class="w-full max-w-md">
          <div class="flex flex-col items-center mb-8">
            <div class="h-14 w-14 flex items-center justify-center text-gable-green drop-shadow-glow mb-3">
              ${icon(Truck, 40)}
            </div>
            <div class="text-3xl font-bold tracking-tight">
              AI<span class="text-gable-green font-light tracking-[0.2em]">LM</span>
            </div>
            <p class="text-sm text-zinc-500 mt-1">AI Load Management &amp; Compliance</p>
          </div>

          <form @submit=${this._submit} class="glass-card rounded-2xl p-6 space-y-5 shadow-elevation-2">
            <div>
              <h1 class="text-lg font-semibold text-zinc-100">Staff sign in</h1>
              <p class="text-xs text-zinc-500 mt-1">
                Use your GableLBM staff email. Access is granted by your GableLBM entitlements.
              </p>
            </div>

            <label class="flex flex-col gap-1.5 text-xs text-zinc-400">
              Email
              <div class="flex items-center gap-2 bg-slate-steel border border-white/10 rounded-lg px-3 focus-within:ring-1 focus-within:ring-gable-green/50">
                <span class="text-zinc-500">${icon(Mail, 16)}</span>
                <input
                  type="email"
                  name="email"
                  autocomplete="email"
                  required
                  placeholder="you@yourdealer.com"
                  .value=${this._email}
                  @input=${(e: Event) => { this._email = (e.target as HTMLInputElement).value; }}
                  ?disabled=${this._busy}
                  class="flex-1 bg-transparent py-2.5 text-sm font-mono text-white placeholder:text-zinc-600 focus:outline-none disabled:opacity-50"
                />
              </div>
            </label>

            ${this._error
              ? html`<div class="flex items-start gap-2 px-3 py-2.5 rounded-lg border border-safety-red/30 bg-safety-red/10 text-safety-red text-sm">
                  ${icon(AlertTriangle, 16, 'shrink-0 mt-0.5')}
                  <span>${this._error}</span>
                </div>`
              : nothing}

            <button
              type="submit"
              ?disabled=${this._busy || this._email.trim() === ''}
              class="w-full flex items-center justify-center gap-2 bg-gable-green text-deep-space font-semibold px-4 py-2.5 rounded-lg hover:shadow-glow transition-all disabled:opacity-40 disabled:cursor-not-allowed"
            >
              ${icon(LogIn, 18)} ${this._busy ? 'Signing in…' : 'Sign in'}
            </button>
          </form>

          <p class="text-center text-[11px] text-zinc-600 mt-6 font-mono">
            Standalone dispatch module · backed by GableLBM
          </p>
        </div>
      </div>
    `;
  }
}
