// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import { defineConfig } from 'vite'

// https://vite.dev/config/
//
// AI_LM mounts ALL its domain routes under /api/v1/* (plus /health, /metrics),
// so unlike GableLBM/GableRun we need no path rewriting — a single pass-through
// proxy of /api → the backend (:8090) is sufficient.
export default defineConfig({
  build: {
    sourcemap: false,
    target: 'es2022',
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (id.includes('node_modules')) {
            if (id.includes('/lit/') || id.includes('/@lit/') || id.includes('/lit-html/') || id.includes('/@lit/reactive-element/')) {
              return 'vendor-lit';
            }
            if (id.includes('/chart.js/')) return 'vendor-chartjs';
            if (id.includes('/lucide/')) return 'vendor-icons';
            if (id.includes('/leaflet/')) return 'vendor-leaflet';
            if (id.includes('/three/')) return 'vendor-three';
          }
        },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: process.env.VITE_API_PROXY || 'http://localhost:8090',
        changeOrigin: false,
      },
    },
  },
})
