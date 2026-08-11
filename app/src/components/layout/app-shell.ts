// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import { LitElement, html, nothing } from 'lit';
import { customElement, state, property } from 'lit/decorators.js';
import { router } from '../../lib/router.ts';
import { icon } from '../../lib/icons.ts';
import { Truck, Sparkles, ShieldAlert, ChevronLeft, ChevronRight, LogOut } from 'lucide';

// Self-contained ERP-style shell for AI_LM. Mirrors GableRun's gable-app-shell
// look (Industrial Dark, collapsible sidebar) without its enterprise widgets.
@customElement('ailm-app-shell')
export class AiLmAppShell extends LitElement {
  createRenderRoot() { return this; }

  @property({ attribute: false }) pageContent: unknown = nothing;
  @state() private _sidebarOpen = true;
  private _boundRouteChanged = () => { this.requestUpdate(); };

  connectedCallback() {
    super.connectedCallback();
    router.addEventListener('route-changed', this._boundRouteChanged);
  }
  disconnectedCallback() {
    super.disconnectedCallback();
    router.removeEventListener('route-changed', this._boundRouteChanged);
  }

  // Clear the AI_LM session and return to the standalone login page.
  private _signOut() {
    localStorage.removeItem('token');
    localStorage.removeItem('ailm_name');
    window.location.href = '/login';
  }

  private _initials(name: string): string {
    const parts = name.trim().split(/\s+/).filter(Boolean);
    if (parts.length === 0) return 'AD';
    if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
    return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
  }

  private _navItem(to: string, iconData: Parameters<typeof icon>[0], label: string) {
    const path = router.currentPath;
    const active = path === to || path.startsWith(to + '/');
    return html`
      <a href="${to}" class="flex items-center gap-3 px-3 py-2.5 rounded-lg transition-all duration-200 text-sm font-medium group relative overflow-hidden ${active
        ? 'text-gable-green bg-gable-green/10 shadow-[inset_0_0_0_1px_rgba(0,255,163,0.2)]'
        : 'text-zinc-400 hover:text-white hover:bg-white/5'}">
        ${active ? html`<div class="absolute left-0 top-2 bottom-2 w-1 bg-gable-green rounded-r-full"></div>` : nothing}
        <span class="relative z-10">${icon(iconData, 20)}</span>
        ${this._sidebarOpen ? html`<span class="whitespace-nowrap relative z-10">${label}</span>` : nothing}
      </a>
    `;
  }

  render() {
    const w = this._sidebarOpen ? 280 : 80;
    const name = localStorage.getItem('ailm_name') || '';
    return html`
      <div class="min-h-screen bg-deep-space text-foreground flex overflow-hidden font-sans selection:bg-gable-green/30">
        <aside
          class="bg-slate-steel border-r border-white/5 flex flex-col fixed inset-y-0 left-0 z-50 shadow-elevation-2 transition-all duration-300"
          style="width: ${w}px"
        >
          <div class="h-16 flex items-center px-4 border-b border-white/5 bg-deep-space/20 gap-3 overflow-hidden">
            <div class="h-10 w-10 flex items-center justify-center shrink-0 text-gable-green drop-shadow-glow">
              ${icon(Truck, 26)}
            </div>
            ${this._sidebarOpen
              ? html`<div class="text-xl font-bold tracking-tight">AI<span class="text-gable-green font-light tracking-[0.2em]">LM</span></div>`
              : nothing}
          </div>

          <nav aria-label="Main navigation" class="flex-1 p-3 space-y-1 overflow-y-auto">
            ${this._sidebarOpen
              ? html`<div class="mb-2 px-3 text-xs font-semibold text-zinc-500 uppercase tracking-wider">Operations</div>`
              : nothing}
            ${this._navItem('/plan', Sparkles, 'Load Planner')}
            ${this._sidebarOpen
              ? html`<div class="mb-2 mt-4 px-3 text-xs font-semibold text-zinc-500 uppercase tracking-wider">Configuration</div>`
              : nothing}
            ${this._navItem('/fleet', Truck, 'Fleet Profiles')}
            ${this._navItem('/compliance', ShieldAlert, 'Compliance')}
          </nav>

          <button
            @click=${() => { this._sidebarOpen = !this._sidebarOpen; }}
            aria-label="${this._sidebarOpen ? 'Collapse sidebar' : 'Expand sidebar'}"
            class="absolute -right-3 top-20 bg-slate-steel border border-white/10 rounded-full text-zinc-400 hover:text-white shadow-elevation-1 hover:shadow-glow transition-all duration-200 z-50 flex items-center justify-center w-6 h-6"
          >
            ${icon(this._sidebarOpen ? ChevronLeft : ChevronRight, 12)}
          </button>
        </aside>

        <main
          class="flex-1 flex flex-col min-h-screen relative w-full transition-all duration-300"
          style="margin-left: ${w}px"
        >
          <header class="h-16 border-b border-white/5 bg-deep-space/80 backdrop-blur-xl px-6 flex items-center justify-between sticky top-0 z-40">
            <div class="text-sm text-zinc-400 font-medium">AI Load Management &amp; Compliance</div>
            <div class="flex items-center gap-3">
              ${name
                ? html`<span class="hidden sm:inline text-xs text-zinc-400 font-medium">${name}</span>`
                : nothing}
              <div class="h-9 w-9 rounded-full bg-gradient-to-br from-gable-green/20 to-emerald-500/20 border border-gable-green/30 flex items-center justify-center text-xs font-mono font-bold text-gable-green shadow-glow">
                ${this._initials(name)}
              </div>
              <button
                @click=${this._signOut}
                title="Sign out"
                aria-label="Sign out"
                class="flex items-center gap-1.5 text-xs text-zinc-400 hover:text-safety-red border border-white/10 hover:border-safety-red/40 rounded-lg px-2.5 py-1.5 transition-all"
              >
                ${icon(LogOut, 16)}
                <span class="hidden sm:inline">Sign out</span>
              </button>
            </div>
          </header>

          <div id="main-content" class="p-6 md:p-8 w-full animate-fade-in">
            ${this.pageContent}
          </div>
        </main>
      </div>
    `;
  }
}
