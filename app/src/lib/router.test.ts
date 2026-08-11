// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { router, type RouteConfig, type RouteMatch } from './router.ts';
import { routes as appRoutes } from '../routes.ts';

const testRoutes: RouteConfig[] = [
  { path: '/', redirect: '/plan', layout: 'app' },
  { path: '/plan', layout: 'app' },
  { path: '/fleet', layout: 'app' },
  { path: '/fleet/:vehicleId', layout: 'app' },
  { path: '/login', layout: 'none' },
];

/** Captures every route-changed payload emitted while `fn` runs. */
async function captureMatches(fn: () => void): Promise<(RouteMatch | null)[]> {
  const seen: (RouteMatch | null)[] = [];
  const listener = (e: Event) => seen.push((e as CustomEvent<RouteMatch | null>).detail);
  router.addEventListener('route-changed', listener);
  fn();
  router.removeEventListener('route-changed', listener);
  return seen;
}

beforeEach(() => {
  window.history.replaceState({}, '', '/plan');
});

describe('router — matching', () => {
  it('resolves the current path on init and reports the match', () => {
    window.history.replaceState({}, '', '/fleet');
    router.init(testRoutes);

    expect(router.currentMatch?.route.path).toBe('/fleet');
    expect(router.currentPath).toBe('/fleet');
  });

  it('captures and URL-decodes dynamic segments', () => {
    window.history.replaceState({}, '', '/fleet/veh%20one');
    router.init(testRoutes);

    expect(router.currentMatch?.route.path).toBe('/fleet/:vehicleId');
    expect(router.currentMatch?.params).toEqual({ vehicleId: 'veh one' });
  });

  it('does not let a dynamic route swallow a shorter path', () => {
    window.history.replaceState({}, '', '/fleet');
    router.init(testRoutes);

    expect(router.currentMatch?.route.path).toBe('/fleet');
    expect(router.currentMatch?.params).toEqual({});
  });

  it('reports no match for an unknown path', () => {
    window.history.replaceState({}, '', '/does-not-exist');
    router.init(testRoutes);

    expect(router.currentMatch).toBeNull();
  });

  it('does not match the root pattern against a deeper path', () => {
    window.history.replaceState({}, '', '/plan/extra');
    router.init(testRoutes);

    expect(router.currentMatch).toBeNull();
  });

  it('follows a redirect route and rewrites the URL without a history entry', () => {
    window.history.replaceState({}, '', '/');
    const lengthBefore = window.history.length;

    router.init(testRoutes);

    expect(window.location.pathname).toBe('/plan');
    expect(router.currentMatch?.route.path).toBe('/plan');
    expect(window.history.length).toBe(lengthBefore);
  });
});

describe('router — navigation', () => {
  it('pushes a history entry and announces the new match', async () => {
    router.init(testRoutes);

    const seen = await captureMatches(() => router.navigate('/login'));

    expect(window.location.pathname).toBe('/login');
    expect(seen).toHaveLength(1);
    expect(seen[0]?.route.layout).toBe('none');
  });

  it('ignores a navigation to the path already showing', async () => {
    router.init(testRoutes);

    const seen = await captureMatches(() => router.navigate('/plan'));

    expect(seen).toHaveLength(0);
  });

  it('announces a null match when navigating somewhere unrouted', async () => {
    router.init(testRoutes);

    const seen = await captureMatches(() => router.navigate('/nope'));

    expect(seen).toEqual([null]);
  });
});

describe('router — anchor interception', () => {
  /**
   * Dispatches a click and reports whether the router claimed it. A guard
   * listener registered after the router's own (so it runs second in the bubble
   * phase) records the verdict and then cancels the event unconditionally,
   * because jsdom would otherwise try a real page navigation.
   */
  function clickAnchor(a: HTMLAnchorElement, init: MouseEventInit = {}): boolean {
    let intercepted = false;
    const guard = (e: Event) => {
      intercepted = e.defaultPrevented;
      e.preventDefault();
    };
    document.addEventListener('click', guard);
    try {
      a.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, ...init }));
    } finally {
      document.removeEventListener('click', guard);
    }
    return intercepted;
  }

  function anchor(href: string, attrs: Record<string, string> = {}): HTMLAnchorElement {
    const a = document.createElement('a');
    a.setAttribute('href', href);
    for (const [k, v] of Object.entries(attrs)) a.setAttribute(k, v);
    document.body.append(a);
    return a;
  }

  it('handles an in-app link without a full page load', () => {
    router.init(testRoutes);

    expect(clickAnchor(anchor('/fleet'))).toBe(true);
    expect(window.location.pathname).toBe('/fleet');
  });

  it('leaves an off-site link to the browser', () => {
    router.init(testRoutes);

    expect(clickAnchor(anchor('https://openrouteservice.org/'))).toBe(false);
    expect(window.location.pathname).toBe('/plan');
  });

  it('leaves downloads, hash links, mailto and new-tab links to the browser', () => {
    router.init(testRoutes);

    expect(clickAnchor(anchor('/fleet', { download: 'manifest.pdf' }))).toBe(false);
    expect(clickAnchor(anchor('#axles'))).toBe(false);
    expect(clickAnchor(anchor('mailto:dispatch@yourdealer.com'))).toBe(false);
    expect(clickAnchor(anchor('/fleet', { target: '_blank' }))).toBe(false);
    expect(window.location.pathname).toBe('/plan');
  });

  it('leaves a modifier-click (open in new tab) to the browser', () => {
    router.init(testRoutes);

    expect(clickAnchor(anchor('/fleet'), { metaKey: true })).toBe(false);
    expect(clickAnchor(anchor('/fleet'), { ctrlKey: true })).toBe(false);
    expect(window.location.pathname).toBe('/plan');
  });
});

describe('routes table', () => {
  it('redirects the retired Dispatch Board and Load Builder paths into /plan', () => {
    const redirects = appRoutes.filter((r) => r.redirect);
    expect(redirects.map((r) => r.path).sort()).toEqual(['/', '/dispatch', '/load']);
    expect(redirects.every((r) => r.redirect === '/plan')).toBe(true);
    expect(redirects.every((r) => r.load === undefined)).toBe(true);
  });

  it('lazy-loads every renderable page and keeps login outside the app shell', () => {
    const renderable = appRoutes.filter((r) => !r.redirect);
    expect(renderable.every((r) => typeof r.load === 'function')).toBe(true);
    expect(renderable.find((r) => r.path === '/login')?.layout).toBe('none');
    expect(
      renderable.filter((r) => r.path !== '/login').every((r) => r.layout === 'app'),
    ).toBe(true);
  });
});

describe('router — popstate', () => {
  it('re-resolves when the browser goes back', async () => {
    router.init(testRoutes);
    router.navigate('/fleet');

    const seen = await captureMatches(() => {
      window.history.replaceState({}, '', '/login');
      window.dispatchEvent(new PopStateEvent('popstate'));
    });

    expect(seen).toHaveLength(1);
    expect(seen[0]?.route.path).toBe('/login');
  });
});

describe('router — singleton', () => {
  it('is the same instance every importer sees', async () => {
    const again = (await import('./router.ts')).router;
    expect(again).toBe(router);
    expect(vi.isMockFunction(router.navigate)).toBe(false);
  });
});
