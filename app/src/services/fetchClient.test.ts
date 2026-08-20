// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * fetchWithAuth is the single door every AI_LM request goes through, including
 * the ERP write-back (`POST /workflow/plans/{id}/push`). Its auth, timeout and
 * retry behaviour therefore decides whether a dispatcher action happens once,
 * twice, or not at all.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { fetchWithAuth } from './fetchClient.ts';

function headersOf(mock: ReturnType<typeof vi.fn>, call = 0): Headers {
  const init = mock.mock.calls[call][1] as RequestInit;
  return init.headers as Headers;
}

/** Resolves with the error a request rejected with, failing if it succeeds. */
async function rejection(pending: Promise<unknown>): Promise<Error> {
  try {
    await pending;
  } catch (err) {
    return err as Error;
  }
  throw new Error('expected the request to reject');
}

/** A fetch that never settles until its AbortSignal fires. */
function hangingFetch() {
  return vi.fn(
    (_url: string, init: RequestInit) =>
      new Promise<Response>((_resolve, reject) => {
        init.signal?.addEventListener('abort', () => {
          const err = new Error('The operation was aborted.');
          err.name = 'AbortError';
          reject(err);
        });
      }),
  );
}

afterEach(() => {
  vi.useRealTimers();
});

describe('fetchWithAuth — authorization', () => {
  it('attaches the stored session token as a bearer header', async () => {
    localStorage.setItem('token', 'session-abc');
    const fetchMock = vi.fn().mockResolvedValue(new Response('{}', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);

    await fetchWithAuth('/api/v1/fleet/profiles');

    expect(headersOf(fetchMock).get('Authorization')).toBe('Bearer session-abc');
  });

  it('sends no Authorization header when the operator is not signed in', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('{}', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);

    await fetchWithAuth('/api/v1/fleet/profiles');

    expect(headersOf(fetchMock).has('Authorization')).toBe(false);
  });

  it('does not overwrite an Authorization header the caller supplied', async () => {
    localStorage.setItem('token', 'session-abc');
    const fetchMock = vi.fn().mockResolvedValue(new Response('{}', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);

    await fetchWithAuth('/api/v1/fleet/profiles', {
      headers: { Authorization: 'Bearer integration-key' },
    });

    expect(headersOf(fetchMock).get('Authorization')).toBe('Bearer integration-key');
  });
});

describe('fetchWithAuth — request shaping', () => {
  it('declares a JSON content type for string bodies', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('{}', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);

    await fetchWithAuth('/api/v1/workflow/plans', {
      method: 'POST',
      body: JSON.stringify({ date: '2026-08-12' }),
    });

    expect(headersOf(fetchMock).get('Content-Type')).toBe('application/json');
    expect((fetchMock.mock.calls[0][1] as RequestInit).method).toBe('POST');
  });

  it('leaves the content type unset on bodyless requests', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('{}', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);

    await fetchWithAuth('/api/v1/workflow/plans/p1/push', { method: 'POST' });

    expect(headersOf(fetchMock).has('Content-Type')).toBe(false);
  });

  it('hands non-OK responses back to the caller instead of throwing', async () => {
    // The service layer (jsonOrThrow) owns error mapping; the transport must
    // not swallow a 502 or the "PIM outage" envelope never reaches the UI.
    const fetchMock = vi.fn().mockResolvedValue(new Response('{}', { status: 502 }));
    vi.stubGlobal('fetch', fetchMock);

    const res = await fetchWithAuth('/api/v1/catalog/products');

    expect(res.status).toBe(502);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

describe('fetchWithAuth — session expiry', () => {
  it('drops the stored token when the backend rejects the session', async () => {
    window.history.replaceState({}, '', '/login');
    localStorage.setItem('token', 'stale');
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 401 }));
    vi.stubGlobal('fetch', fetchMock);
    vi.useFakeTimers();

    const settled = rejection(fetchWithAuth('/api/v1/fleet/profiles'));
    await vi.advanceTimersByTimeAsync(2_500);

    expect((await settled).message).toBe('Session expired');
    expect(localStorage.getItem('token')).toBeNull();
  });

  // Session expiry is a TERMINAL condition, not a transient failure. Throwing
  // 'Session expired' from inside the try would let the generic catch sleep
  // RETRY_DELAY (2s) and re-issue the request — a duplicate call plus a 2s
  // stall on every expired session, and because the same wrapper carries
  // POST /workflow/plans/{id}/push, a duplicated ERP write-back. The 401 check
  // therefore lives outside the try, so it leaves the retry loop immediately.
  it('does not re-issue the request after a 401', async () => {
    window.history.replaceState({}, '', '/login');
    localStorage.setItem('token', 'stale');
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 401 }));
    vi.stubGlobal('fetch', fetchMock);
    vi.useFakeTimers();

    const settled = fetchWithAuth('/api/v1/workflow/plans/p1/push', {
      method: 'POST',
    }).catch(() => undefined);
    await vi.advanceTimersByTimeAsync(2_500);
    await settled;
    vi.useRealTimers();

    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

describe('fetchWithAuth — retry and cancellation', () => {
  it('retries once after a network failure and returns the retried response', async () => {
    const fetchMock = vi
      .fn()
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockResolvedValueOnce(new Response('{"ok":true}', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    vi.useFakeTimers();

    const pending = fetchWithAuth('/api/v1/fleet/profiles');
    await vi.advanceTimersByTimeAsync(2_500);
    const res = await pending;

    expect(res.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('gives up immediately when the caller cancels', async () => {
    const fetchMock = hangingFetch();
    vi.stubGlobal('fetch', fetchMock);
    const controller = new AbortController();

    const settled = rejection(
      fetchWithAuth('/api/v1/routing/plan', { signal: controller.signal }),
    );
    controller.abort();

    expect((await settled).name).toBe('AbortError');
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('aborts the in-flight request once the timeout elapses', async () => {
    const fetchMock = hangingFetch();
    vi.stubGlobal('fetch', fetchMock);
    vi.useFakeTimers();

    const settled = rejection(
      fetchWithAuth('/api/v1/load/optimize', { timeout: 50, retries: 0 }),
    );
    await vi.advanceTimersByTimeAsync(60);

    expect((await settled).name).toBe('AbortError');
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
