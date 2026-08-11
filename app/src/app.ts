// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import { LitElement, html } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { router, type RouteMatch } from './lib/router.ts';

import './components/layout/app-shell.ts';

@customElement('ailm-app')
export class AiLmApp extends LitElement {
  // Light DOM so Tailwind utility classes apply.
  createRenderRoot() { return this; }

  @state() private _match: RouteMatch | null = null;
  @state() private _loading = true;

  connectedCallback() {
    super.connectedCallback();
    router.addEventListener('route-changed', this._onRouteChanged);
    if (router.currentMatch) {
      this._onRouteChanged(new CustomEvent('route-changed', { detail: router.currentMatch }));
    }
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    router.removeEventListener('route-changed', this._onRouteChanged);
  }

  private _onRouteChanged = async (e: Event) => {
    const match = (e as CustomEvent<RouteMatch | null>).detail;
    this._match = match;
    if (!match) {
      this._loading = false;
      return;
    }
    this._loading = true;
    try {
      await match.route.load?.();
    } catch (err) {
      console.error('Failed to load route:', err);
    } finally {
      this._loading = false;
    }
  };

  private _pathToTag(path: string): string {
    const tagMap: Record<string, string> = {
      '/login': 'ailm-login',
      '/plan': 'ailm-plan-workflow',
      '/fleet': 'ailm-fleet-profiles',
      '/compliance': 'ailm-compliance-points',
    };
    return tagMap[path] || 'ailm-not-found';
  }

  private _renderPageTag(tag: string) {
    const params = this._match?.params || {};
    const el = document.createElement(tag);
    for (const [key, value] of Object.entries(params)) {
      el.setAttribute(`route-${key}`, value);
    }
    return html`${el}`;
  }

  render() {
    if (this._loading) {
      return html`
        <div class="flex h-screen w-full items-center justify-center bg-deep-space">
          <div class="flex flex-col items-center gap-3">
            <div class="h-8 w-8 animate-spin rounded-full border-2 border-gable-green border-t-transparent"></div>
            <span class="text-sm text-zinc-500 font-medium tracking-wide">Loading…</span>
          </div>
        </div>
      `;
    }

    if (!this._match) {
      return html`
        <div class="flex h-screen w-full items-center justify-center bg-deep-space text-white">
          <div class="text-center">
            <h1 class="text-4xl font-bold font-mono mb-2">404</h1>
            <p class="text-zinc-400">Page not found</p>
            <a href="/plan" class="text-gable-green hover:underline mt-4 inline-block">Go to the Load Planner</a>
          </div>
        </div>
      `;
    }

    const tag = this._pathToTag(this._match.route.path);
    const pageHtml = this._renderPageTag(tag);

    if (this._match.route.layout === 'none') {
      return html`${pageHtml}`;
    }
    return html`<ailm-app-shell .pageContent=${pageHtml}></ailm-app-shell>`;
  }
}
