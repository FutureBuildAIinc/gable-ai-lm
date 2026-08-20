// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * Shared fetch wrapper with auth header, timeout, and retry logic.
 * Drop-in replacement for fetch() — all frontend services should use this.
 * Simplified from GableRun: AI_LM has no branch context, so no X-Branch-Id.
 */

const DEFAULT_TIMEOUT = 15_000; // 15 seconds (route/load solves can be slower)
const DEFAULT_RETRIES = 1;
const RETRY_DELAY = 2_000;

export interface FetchWithAuthOptions extends Omit<RequestInit, 'signal'> {
  timeout?: number;
  retries?: number;
  signal?: AbortSignal | null;
}

export async function fetchWithAuth(
  url: string,
  options: FetchWithAuthOptions = {}
): Promise<Response> {
  const {
    timeout = DEFAULT_TIMEOUT,
    retries = DEFAULT_RETRIES,
    headers: customHeaders,
    signal: externalSignal,
    ...fetchOpts
  } = options;

  const headers = new Headers(customHeaders);

  // Inject auth token if present (AUTH_MODE=dev backends ignore it).
  if (!headers.has('Authorization')) {
    const token = localStorage.getItem('token');
    if (token) {
      headers.set('Authorization', `Bearer ${token}`);
    }
  }

  if (!headers.has('Content-Type') && fetchOpts.body && typeof fetchOpts.body === 'string') {
    headers.set('Content-Type', 'application/json');
  }

  let lastError: Error | null = null;

  for (let attempt = 0; attempt <= retries; attempt++) {
    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), timeout);

    if (externalSignal != null) {
      externalSignal.addEventListener('abort', () => controller.abort(), { once: true });
    }

    let response: Response;
    try {
      response = await fetch(url, {
        ...fetchOpts,
        headers,
        signal: controller.signal,
      });
      clearTimeout(timeoutId);
    } catch (err) {
      clearTimeout(timeoutId);
      lastError = err instanceof Error ? err : new Error(String(err));

      if (externalSignal?.aborted) {
        throw lastError;
      }

      if (attempt < retries && lastError.name !== 'AbortError') {
        await new Promise((resolve) => setTimeout(resolve, RETRY_DELAY));
      }
      continue;
    }

    // Session expiry is TERMINAL, not transient. This throw deliberately sits
    // outside the try above: raising it from inside would let the generic
    // network-failure catch sleep RETRY_DELAY and re-issue the request against
    // the token we just discarded. This wrapper also carries
    // POST /workflow/plans/{id}/push, so a retried 401 fires the ERP
    // write-back twice.
    if (response.status === 401) {
      localStorage.removeItem('token');
      const path = window.location.pathname;
      if (!path.endsWith('/login')) {
        window.location.href = '/login';
      }
      throw new Error('Session expired');
    }

    return response;
  }

  throw lastError!;
}
