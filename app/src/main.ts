// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import './index.css';
import 'leaflet/dist/leaflet.css';
import { router } from './lib/router.ts';
import { routes } from './routes.ts';
import './app.ts';

// Mount the app into #root (replacing the loading spinner from index.html).
const root = document.getElementById('root');
if (root) {
  root.innerHTML = '<ailm-app></ailm-app>';
}

router.init(routes);
