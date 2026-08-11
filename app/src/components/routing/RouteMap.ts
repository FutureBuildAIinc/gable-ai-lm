// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import { LitElement, html } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import * as L from 'leaflet';
import type { ComplianceFlag, Load } from '../../services/aiLmService.ts';

// Per-load color palette — one distinct route color per truck.
const LOAD_COLORS = ['#00FFA3', '#38BDF8', '#FBBF24', '#A78BFA', '#F472B6'];

/**
 * <ailm-route-map> — Leaflet map of a multi-load route plan: each load draws its
 * own colored polyline + numbered stop markers, with restricted-point flags
 * overlaid. Re-fits bounds whenever loads/flags change.
 */
@customElement('ailm-route-map')
export class RouteMap extends LitElement {
  createRenderRoot() { return this; }

  @property({ attribute: false }) loads: Load[] = [];
  @property({ attribute: false }) flags: ComplianceFlag[] = [];
  /** Depot/yard origin — when set, every route polyline starts here. */
  @property({ attribute: false }) depot: { lat: number; lng: number } | null = null;
  /** AI-inserted detour waypoints (from compliance auto-rerouting). */
  @property({ attribute: false }) detours: { lat: number; lng: number }[] = [];

  private _map?: L.Map;
  private _layer?: L.LayerGroup;

  firstUpdated() {
    this._initMap();
    this._draw();
  }

  updated(changed: Map<string, unknown>) {
    if (
      (changed.has('loads') || changed.has('flags') || changed.has('depot') || changed.has('detours')) &&
      this._map
    ) {
      this._draw();
    }
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this._map?.remove();
  }

  private _initMap() {
    const host = this.querySelector('#map-host') as HTMLElement | null;
    if (!host) return;
    this._map = L.map(host, { zoomControl: true, attributionControl: false }).setView([49.88, -119.49], 11);
    L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', {
      maxZoom: 19,
    }).addTo(this._map);
    this._layer = L.layerGroup().addTo(this._map);
  }

  private _stopIcon(seq: number, status: 'ok' | 'warn' | 'fail', loadColor?: string) {
    const color =
      status === 'fail' ? '#F43F5E' : status === 'warn' ? '#FBBF24' : loadColor ?? '#00FFA3';
    return L.divIcon({
      className: 'ailm-stop-marker',
      html: `<div style="background:${color};color:#0A0B10;width:26px;height:26px;border-radius:50%;display:flex;align-items:center;justify-content:center;font-family:'JetBrains Mono',monospace;font-weight:700;font-size:12px;box-shadow:0 0 10px ${color}80;border:2px solid #0A0B10">${seq}</div>`,
      iconSize: [26, 26],
      iconAnchor: [13, 13],
    });
  }

  private _flagIcon(severity: 'WARN' | 'FAIL') {
    const color = severity === 'FAIL' ? '#F43F5E' : '#FBBF24';
    return L.divIcon({
      className: 'ailm-flag-marker',
      html: `<div style="width:0;height:0;border-left:8px solid transparent;border-right:8px solid transparent;border-bottom:16px solid ${color};filter:drop-shadow(0 0 6px ${color})"></div>`,
      iconSize: [16, 16],
      iconAnchor: [8, 16],
    });
  }

  private _draw() {
    if (!this._map || !this._layer) return;
    this._layer.clearLayers();

    const all: L.LatLngExpression[] = [];

    if (this.depot) {
      const d = this.depot;
      all.push([d.lat, d.lng]);
      L.marker([d.lat, d.lng], {
        icon: L.divIcon({
          className: 'ailm-depot-marker',
          html: `<div style="background:#161821;color:#00FFA3;width:30px;height:30px;border-radius:6px;display:flex;align-items:center;justify-content:center;font-family:'JetBrains Mono',monospace;font-weight:700;font-size:13px;border:2px solid #00FFA3;box-shadow:0 0 12px #00FFA380">Y</div>`,
          iconSize: [30, 30],
          iconAnchor: [15, 15],
        }),
      })
        .bindPopup('<b>Yard / Depot</b>')
        .addTo(this._layer!);
    }

    this.loads.forEach((load, li) => {
      const color = LOAD_COLORS[li % LOAD_COLORS.length];
      const latlngs: L.LatLngExpression[] = load.stops.map((s) => [s.lat, s.lng]);
      all.push(...latlngs);

      const path: L.LatLngExpression[] = this.depot
        ? [[this.depot.lat, this.depot.lng], ...latlngs]
        : latlngs;
      if (path.length > 1) {
        L.polyline(path, { color, weight: 3, opacity: 0.7, dashArray: '6 6' }).addTo(this._layer!);
      }

      load.stops.forEach((s) => {
        L.marker([s.lat, s.lng], { icon: this._stopIcon(s.sequence, 'ok', color) })
          .bindPopup(
            `<b>${load.vehicle_name} — Stop ${s.sequence}</b><br/>${s.address || s.order_id}<br/>${s.weight_lbs.toLocaleString()} lbs`,
          )
          .addTo(this._layer!);
      });
    });

    this.flags.forEach((f) => {
      L.marker([f.point.lat, f.point.lng], { icon: this._flagIcon(f.severity) })
        .bindPopup(`<b>${f.point.name}</b><br/>${f.violation}<br/><i>${f.severity}</i>`)
        .addTo(this._layer!);
    });

    this.detours.forEach((d) => {
      all.push([d.lat, d.lng]);
      L.circleMarker([d.lat, d.lng], {
        radius: 8,
        color: '#38BDF8',
        weight: 2,
        dashArray: '3 3',
        fillColor: '#38BDF8',
        fillOpacity: 0.25,
      })
        .bindPopup('<b>AI detour waypoint</b><br/>Inserted to route around a restriction')
        .addTo(this._layer!);
    });

    all.push(...this.flags.map((f) => [f.point.lat, f.point.lng] as L.LatLngExpression));
    if (all.length > 0) {
      this._map.fitBounds(L.latLngBounds(all).pad(0.2));
    }
  }

  render() {
    return html`
      <div class="relative w-full h-[480px] rounded-xl overflow-hidden border border-white/5">
        <div id="map-host" class="absolute inset-0"></div>
      </div>
    `;
  }
}
