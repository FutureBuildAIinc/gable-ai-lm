// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * <ailm-app> maps a resolved route onto a page element and decides whether the
 * ERP shell wraps it; <ailm-app-shell> owns navigation and the session badge.
 *
 * The page elements themselves are deliberately NOT imported here, so they stay
 * unregistered and render as inert placeholders — this file is about the
 * routing/layout wiring, not about the pages.
 */
import { beforeEach, describe, expect, it } from 'vitest';
import type { AiLmApp } from './app.ts';
import './app.ts';
import type { AiLmAppShell } from './components/layout/app-shell.ts';
import { router, type RouteConfig } from './lib/router.ts';
import { flush, mount, text } from './test/dom.ts';

const testRoutes: RouteConfig[] = [
  { path: '/plan', layout: 'app' },
  { path: '/fleet', layout: 'app' },
  { path: '/compliance', layout: 'app' },
  { path: '/login', layout: 'none' },
  { path: '/fleet/:vehicleId', layout: 'app' },
];

/** Mirrors main.ts: mount the app first, then hand the router its table. */
async function mountAppAt(path: string): Promise<AiLmApp> {
  window.history.replaceState({}, '', path);
  const el = await mount<AiLmApp>('ailm-app');
  router.init(testRoutes);
  await flush();
  await el.updateComplete;
  return el;
}

beforeEach(() => {
  window.history.replaceState({}, '', '/plan');
});

describe('AiLmApp — layout selection', () => {
  it.each([
    ['/plan', 'ailm-plan-workflow'],
    ['/fleet', 'ailm-fleet-profiles'],
    ['/compliance', 'ailm-compliance-points'],
  ])('renders %s inside the ERP shell as <%s>', async (path, tag) => {
    const el = await mountAppAt(path);

    const shell = el.querySelector('ailm-app-shell');
    expect(shell).not.toBeNull();
    expect(shell!.querySelector(tag)).not.toBeNull();
  });

  it('renders the login page bare, with no shell around it', async () => {
    const el = await mountAppAt('/login');

    expect(el.querySelector('ailm-app-shell')).toBeNull();
    expect(el.querySelector('ailm-login')).not.toBeNull();
  });

  it('shows a 404 with a way back to the planner for an unrouted path', async () => {
    const el = await mountAppAt('/nowhere');

    expect(text(el)).toContain('404');
    expect(text(el)).toContain('Page not found');
    expect(el.querySelector('a')?.getAttribute('href')).toBe('/plan');
  });

  it('falls through to not-found for a route the tag map does not know', async () => {
    // _pathToTag is keyed by literal route path, so a route added to routes.ts
    // without a matching tag entry resolves but renders nothing usable. Worth
    // pinning: today no dynamic route ships, and this is what would happen.
    const el = await mountAppAt('/fleet/veh-7');

    expect(el.querySelector('ailm-app-shell')!.querySelector('ailm-not-found')).not.toBeNull();
  });

  it('follows later navigation without a remount', async () => {
    const el = await mountAppAt('/plan');

    router.navigate('/compliance');
    await flush();
    await el.updateComplete;

    expect(el.querySelector('ailm-compliance-points')).not.toBeNull();
    expect(el.querySelector('ailm-plan-workflow')).toBeNull();
  });
});

describe('AiLmAppShell', () => {
  async function shell(): Promise<AiLmAppShell> {
    router.init(testRoutes);
    return mount<AiLmAppShell>('ailm-app-shell');
  }

  it('highlights the nav entry for the page being shown', async () => {
    window.history.replaceState({}, '', '/fleet');
    const el = await shell();

    const links = Array.from(el.querySelectorAll('nav a'));
    const active = links.filter((a) => a.className.includes('text-gable-green'));
    expect(active).toHaveLength(1);
    expect(active[0].getAttribute('href')).toBe('/fleet');
  });

  it('derives an avatar from the signed-in staff name', async () => {
    localStorage.setItem('ailm_name', 'Marc Tremblay');
    const el = await shell();

    expect(text(el.querySelector('header'))).toContain('Marc Tremblay');
    expect(text(el.querySelector('header'))).toContain('MT');
  });

  it('falls back to a placeholder avatar when nobody is signed in', async () => {
    const el = await shell();
    expect(text(el.querySelector('header'))).toContain('AD');
  });

  it('collapses and re-expands the sidebar', async () => {
    const el = await shell();
    const aside = el.querySelector('aside') as HTMLElement;
    expect(aside.getAttribute('style')).toContain('width: 280px');

    const toggle = el.querySelector('[aria-label="Collapse sidebar"]') as HTMLButtonElement;
    toggle.click();
    await el.updateComplete;

    expect((el.querySelector('aside') as HTMLElement).getAttribute('style')).toContain(
      'width: 80px',
    );
    expect(el.querySelector('[aria-label="Expand sidebar"]')).not.toBeNull();
  });

  it('clears both session keys on sign out', async () => {
    localStorage.setItem('token', 'session-abc');
    localStorage.setItem('ailm_name', 'Marc Tremblay');
    const el = await shell();

    // jsdom logs "Not implemented: navigation" for the /login redirect the
    // handler asks for; the session teardown is the part that matters here.
    (el.querySelector('[aria-label="Sign out"]') as HTMLButtonElement).click();
    await flush();

    expect(localStorage.getItem('token')).toBeNull();
    expect(localStorage.getItem('ailm_name')).toBeNull();
  });
});
