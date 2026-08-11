// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * Global test setup: jsdom gaps + per-test isolation.
 *
 * Custom elements are registered once per module graph, so components leak
 * across test files unless the DOM and browser-global state are reset between
 * tests.
 */
import { afterEach, beforeEach } from 'vitest';

// jsdom has no ResizeObserver; several components observe their host element.
if (!('ResizeObserver' in globalThis)) {
  class ResizeObserverStub {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  }
  (globalThis as unknown as { ResizeObserver: unknown }).ResizeObserver = ResizeObserverStub;
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  document.body.innerHTML = '';
  localStorage.clear();
});
