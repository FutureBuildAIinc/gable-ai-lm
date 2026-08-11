// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * Flat ESLint config for the AI_LM operator UI.
 *
 * CI runs `npm run lint --if-present`, so this file is what turns that gate
 * from a no-op into a real check. It is intentionally type-unaware (no
 * `projectService`): a full type-checked lint duplicates `tsc -b`, which the
 * build already runs, and doubles CI time for the same signal.
 */
import js from '@eslint/js';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  {
    ignores: ['dist/**', 'coverage/**', 'node_modules/**'],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ['src/**/*.ts'],
    languageOptions: {
      globals: { ...globals.browser },
      parserOptions: { ecmaFeatures: { jsx: false } },
    },
    rules: {
      // Lit templates read cleaner with non-null assertions on state that the
      // surrounding guard already proved present; flag the risky ones instead.
      '@typescript-eslint/no-non-null-assertion': 'off',
      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_' },
      ],
      eqeqeq: ['error', 'always', { null: 'ignore' }],
      'no-console': ['warn', { allow: ['warn', 'error'] }],
    },
  },
  {
    // Tests deliberately poke at globals and stub browser APIs.
    files: ['src/**/*.test.ts', 'src/test/**/*.ts'],
    rules: {
      '@typescript-eslint/no-explicit-any': 'off',
    },
  },
  {
    // Build tooling runs in Node, not the browser.
    files: ['*.config.js', '*.config.ts', 'vitest.config.ts'],
    languageOptions: {
      globals: { ...globals.node },
    },
  },
);
