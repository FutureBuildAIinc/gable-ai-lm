// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * Icon helper — renders lucide icons as SVG strings for use in Lit templates.
 * Usage:
 *   import { icon } from '../../lib/icons';
 *   import { Truck } from 'lucide';
 *   html`${icon(Truck)}`
 */
import { html } from 'lit';
import { unsafeHTML } from 'lit/directives/unsafe-html.js';
import { createElement } from 'lucide';

export function icon(
  iconData: Parameters<typeof createElement>[0],
  size: number = 20,
  cls: string = ''
) {
  const el = createElement(iconData);
  el.setAttribute('width', String(size));
  el.setAttribute('height', String(size));
  if (cls) el.setAttribute('class', cls);
  return html`${unsafeHTML(el.outerHTML)}`;
}
