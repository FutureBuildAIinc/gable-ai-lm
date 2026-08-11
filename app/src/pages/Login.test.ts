// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * Login is the only writer of the two localStorage keys the rest of the app
 * reads: `token` (fetchClient's bearer header) and `ailm_name` (the shell
 * avatar and the approver recorded on lock overrides and sign-offs). A key
 * rename here silently signs everyone out or attributes an override to nobody.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Login } from './Login.ts';
import './Login.ts';
import { router } from '../lib/router.ts';
import { flush, jsonResponse, mount, text } from '../test/dom.ts';

function fields(el: Login) {
  return {
    email: el.querySelector('input[type="email"]') as HTMLInputElement,
    submit: el.querySelector('button[type="submit"]') as HTMLButtonElement,
    form: el.querySelector('form') as HTMLFormElement,
  };
}

async function signIn(el: Login, email: string) {
  const { email: input, form } = fields(el);
  input.value = email;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  await el.updateComplete;
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await flush();
  await el.updateComplete;
}

beforeEach(() => {
  window.history.replaceState({}, '', '/login');
  router.init([
    { path: '/login', layout: 'none' },
    { path: '/plan', layout: 'app' },
  ]);
});

describe('Login', () => {
  it('keeps sign-in disabled until an email is entered', async () => {
    vi.stubGlobal('fetch', vi.fn());
    const el = await mount<Login>('ailm-login');

    expect(fields(el).submit.disabled).toBe(true);
  });

  it('posts the email to the GableLBM-backed staff endpoint', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(jsonResponse({ token: 'jwt-1', name: 'Marc Tremblay', roles: ['dispatcher'] }));
    vi.stubGlobal('fetch', fetchMock);
    const el = await mount<Login>('ailm-login');

    await signIn(el, '  marc@yourdealer.com  ');

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/auth/login');
    expect(init.method).toBe('POST');
    // The form trims before submitting so a pasted address still validates.
    expect(JSON.parse(init.body as string)).toEqual({ email: 'marc@yourdealer.com' });
  });

  it('stores the session under the keys the rest of the app reads', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse({ token: 'jwt-1', name: 'Marc Tremblay', roles: ['dispatcher'] }),
      ),
    );
    const el = await mount<Login>('ailm-login');

    await signIn(el, 'marc@yourdealer.com');

    expect(localStorage.getItem('token')).toBe('jwt-1');
    expect(localStorage.getItem('ailm_name')).toBe('Marc Tremblay');
    expect(window.location.pathname).toBe('/plan');
  });

  it('shows the entitlement failure and stays put', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(
          { error: { code: 'forbidden', message: 'staff member is not entitled to AI_LM' } },
          403,
        ),
      ),
    );
    const el = await mount<Login>('ailm-login');

    await signIn(el, 'nobody@yourdealer.com');

    expect(text(el)).toContain('staff member is not entitled to AI_LM');
    expect(localStorage.getItem('token')).toBeNull();
    expect(window.location.pathname).toBe('/login');
    expect(fields(el).submit.disabled).toBe(false);
  });

  it('does not stash a name when the backend returns none', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse({ token: 'jwt-1', name: '', roles: [] })),
    );
    const el = await mount<Login>('ailm-login');

    await signIn(el, 'marc@yourdealer.com');

    expect(localStorage.getItem('token')).toBe('jwt-1');
    expect(localStorage.getItem('ailm_name')).toBeNull();
  });
});
