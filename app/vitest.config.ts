// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import { defineConfig } from 'vitest/config';

// Unit/component test config for the AI_LM operator UI.
//
// The suite runs in jsdom because the highest-value tests here are the ones
// that assert what a dispatcher actually SEES: the app renders weights, axle
// utilisation and PASS/WARN/FAIL compliance status, so a formatting bug is a
// safety bug. Those numbers only exist once a Lit component has rendered.
//
// Coverage is reported, never enforced — a threshold gate on a young suite
// pushes contributors toward padding rather than toward the load-safety paths
// that matter.
export default defineConfig({
  test: {
    environment: 'jsdom',
    globals: false,
    include: ['src/**/*.test.ts'],
    setupFiles: ['src/test/setup.ts'],
    restoreMocks: true,
    unstubGlobals: true,
    coverage: {
      provider: 'v8',
      reporter: ['text', 'html', 'lcov'],
      reportsDirectory: 'coverage',
      include: ['src/**/*.ts'],
      exclude: [
        'src/**/*.test.ts',
        'src/test/**',
        'src/main.ts', // bootstrap-only: mounts <ailm-app> and inits the router
      ],
    },
  },
});
