// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

import { LitElement, html } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import * as THREE from 'three';
import { OrbitControls } from 'three/examples/jsm/controls/OrbitControls.js';
import type { LoadPlan } from '../../services/aiLmService.ts';

export interface BedDims {
  length_in: number;
  width_in: number;
  height_in: number;
}

// Color-codes placed boxes by axle group so yard staff can see at a glance how
// each item contributes to a given axle's load. Index = axle number (1-based).
const AXLE_COLORS = [0x38bdf8, 0x00ffa3, 0xfbbf24, 0xf43f5e, 0xa78bfa, 0xfb923c];

// Per-stop accent palette (matches the route map LOAD_COLORS family).
export const STOP_COLORS = [0x00ffa3, 0x38bdf8, 0xfbbf24, 0xa78bfa, 0xf472b6, 0xfb923c];

// Wood tones for the realistic digital-twin mode: picked per SKU so lumber
// reads as lumber (SPF pale, pressure-treated green-brown, cedar red-brown).
function woodToneFor(sku: string): number {
  const s = sku.toUpperCase();
  if (s.includes('-PT')) return 0x7d8a5a; // pressure treated
  if (s.includes('WRC') || s.includes('CEDAR')) return 0xa9714f; // western red cedar
  if (s.includes('OSB') || s.includes('PLY')) return 0xc49a6c; // sheet goods
  return 0xd9b98a; // SPF / whitewood
}

// Small deterministic per-board variation so a bundle doesn't render as one
// flat-colored slab.
function jitterColor(base: number, seed: number): number {
  const f = 0.92 + ((seed * 2654435761) % 100) / 100 * 0.16; // 0.92..1.08
  const r = Math.min(255, Math.round(((base >> 16) & 0xff) * f));
  const g = Math.min(255, Math.round(((base >> 8) & 0xff) * f));
  const b = Math.min(255, Math.round((base & 0xff) * f));
  return (r << 16) | (g << 8) | b;
}

/**
 * <ailm-load-3d-visualizer> — renders the trailer bed and placed boxes in 3D
 * using Three.js. Coordinates from the solver are inches from the front-left-
 * floor corner (X=length, Y=width, Z=height). Three.js uses Y-up, so we map
 * solver (x,y,z) → three (x, z, y) and scale inches → feet for a sane camera.
 */
@customElement('ailm-load-3d-visualizer')
export class Load3DVisualizer extends LitElement {
  createRenderRoot() { return this; }

  @property({ attribute: false }) plan: LoadPlan | null = null;
  @property({ attribute: false }) bed: BedDims | null = null;

  /**
   * 'axle' — color by axle group (classic load-balance view).
   * 'wood' — realistic lumber digital twins (wood tone per SKU) with a
   *          per-stop accent stripe rendered as the box edge color.
   */
  @property() colorMode: 'axle' | 'wood' = 'axle';

  /**
   * Packing playback: when ≥ 0 only placements with step ≤ visibleSteps render
   * (the placement at exactly visibleSteps glows). -1 shows everything.
   */
  @property({ type: Number }) visibleSteps = -1;

  private _scene?: THREE.Scene;
  private _camera?: THREE.PerspectiveCamera;
  private _renderer?: THREE.WebGLRenderer;
  private _controls?: OrbitControls;
  private _frame = 0;
  private _resizeObserver?: ResizeObserver;

  // Build-once placement registry: playback only toggles visibility/highlight,
  // it never rebuilds the scene (a 700-board load rebuilt per tick froze the
  // tab and leaked GPU buffers).
  private _placed: { mesh: THREE.Mesh; edges: THREE.LineSegments; step: number; mat: THREE.Material; edgeMat: THREE.Material }[] = [];
  private _strapGroup?: THREE.Group;
  private _geoCache = new Map<string, { box: THREE.BoxGeometry; edges: THREE.EdgesGeometry }>();
  private _matCache = new Map<string, THREE.Material>();
  private _highlightMat = new THREE.MeshStandardMaterial({ color: 0x00ffa3, emissive: new THREE.Color(0x00ffa3), emissiveIntensity: 0.55, roughness: 0.6 });
  private _highlightEdgeMat = new THREE.LineBasicMaterial({ color: 0x00ffa3 });
  // Shared digital-twin scaling contract: 1 inch = 1/12 Three.js world unit.
  // GableLBM's PIM preview (<gable-product-twin-3d>) uses the identical factor so
  // a product renders at matching world-unit size in both the PIM and here next
  // to the truck bed. Do not change one without the other.
  private readonly _scale = 1 / 12; // inches → feet

  firstUpdated() {
    this._initScene();
    this._rebuild();
  }

  updated(changed: Map<string, unknown>) {
    if (!this._scene) return;
    if (changed.has('plan') || changed.has('bed') || changed.has('colorMode')) {
      this._rebuild();
    } else if (changed.has('visibleSteps')) {
      this._applyVisibility();
    }
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    cancelAnimationFrame(this._frame);
    this._resizeObserver?.disconnect();
    this._disposeBuilt();
    this._highlightMat.dispose();
    this._highlightEdgeMat.dispose();
    this._controls?.dispose();
    this._renderer?.dispose();
  }

  /** Removes built objects from the scene and frees their GPU resources. */
  private _disposeBuilt() {
    if (this._scene) {
      const removable = this._scene.children.filter((c) => c.userData.built);
      removable.forEach((c) => {
        this._scene!.remove(c);
        // Bed/grid/cage/straps own their geometry+material; placements use caches.
        c.traverse((node) => {
          const obj = node as THREE.Mesh;
          if (!obj.userData.cached) {
            (obj.geometry as THREE.BufferGeometry | undefined)?.dispose?.();
            const m = obj.material as THREE.Material | THREE.Material[] | undefined;
            if (Array.isArray(m)) m.forEach((x) => x.dispose());
            else m?.dispose?.();
          }
        });
      });
    }
    this._placed = [];
    this._geoCache.forEach((g) => { g.box.dispose(); g.edges.dispose(); });
    this._geoCache.clear();
    this._matCache.forEach((m) => m.dispose());
    this._matCache.clear();
  }

  private get _canvasHost(): HTMLElement | null {
    return this.querySelector('#three-host');
  }

  private _initScene() {
    const host = this._canvasHost;
    if (!host) return;

    const width = host.clientWidth || 640;
    const height = host.clientHeight || 420;

    this._scene = new THREE.Scene();
    this._scene.background = new THREE.Color(0x0a0b10);

    this._camera = new THREE.PerspectiveCamera(50, width / height, 0.1, 1000);
    this._camera.position.set(18, 14, 18);

    this._renderer = new THREE.WebGLRenderer({ antialias: true });
    this._renderer.setSize(width, height);
    this._renderer.setPixelRatio(window.devicePixelRatio);
    host.appendChild(this._renderer.domElement);

    this._controls = new OrbitControls(this._camera, this._renderer.domElement);
    this._controls.enableDamping = true;
    this._controls.dampingFactor = 0.08;

    this._scene.add(new THREE.AmbientLight(0xffffff, 0.6));
    const dir = new THREE.DirectionalLight(0xffffff, 0.8);
    dir.position.set(10, 20, 10);
    this._scene.add(dir);

    this._resizeObserver = new ResizeObserver(() => this._onResize());
    this._resizeObserver.observe(host);

    const animate = () => {
      this._frame = requestAnimationFrame(animate);
      this._controls?.update();
      if (this._renderer && this._scene && this._camera) {
        this._renderer.render(this._scene, this._camera);
      }
    };
    animate();
  }

  private _onResize() {
    const host = this._canvasHost;
    if (!host || !this._renderer || !this._camera) return;
    const width = host.clientWidth;
    const height = host.clientHeight;
    if (width === 0 || height === 0) return;
    this._renderer.setSize(width, height);
    this._camera.aspect = width / height;
    this._camera.updateProjectionMatrix();
  }

  /** Clears placement/bed meshes and rebuilds them from current props. */
  private _rebuild() {
    if (!this._scene) return;

    this._disposeBuilt();

    const bed = this.bed;
    if (!bed) return;

    const s = this._scale;
    const bedL = bed.length_in * s;
    const bedW = bed.width_in * s;

    // Ground grid centered under the bed.
    const grid = new THREE.GridHelper(Math.max(bedL, bedW) * 1.6, 24, 0x1e2029, 0x161821);
    grid.userData.built = true;
    this._scene.add(grid);

    // Trailer bed: a translucent slab + wireframe outline. We center the whole
    // scene by translating everything so the bed's front-left corner sits at
    // (-bedL/2, 0, -bedW/2).
    const offX = -bedL / 2;
    const offZ = -bedW / 2;

    const bedGeo = new THREE.BoxGeometry(bedL, 0.15, bedW);
    const bedMat = new THREE.MeshStandardMaterial({ color: 0x252836, transparent: true, opacity: 0.7 });
    const bedMesh = new THREE.Mesh(bedGeo, bedMat);
    bedMesh.position.set(offX + bedL / 2, -0.075, offZ + bedW / 2);
    bedMesh.userData.built = true;
    this._scene.add(bedMesh);

    const railH = (bed.height_in * s) || 4;
    const cageGeo = new THREE.BoxGeometry(bedL, railH, bedW);
    const cage = new THREE.LineSegments(
      new THREE.EdgesGeometry(cageGeo),
      new THREE.LineBasicMaterial({ color: 0x00ffa3, transparent: true, opacity: 0.35 }),
    );
    cage.position.set(offX + bedL / 2, railH / 2, offZ + bedW / 2);
    cage.userData.built = true;
    this._scene.add(cage);

    // Placed boxes — built ONCE per plan. Geometries are cached per box size
    // (a load has few distinct board dims) and materials per color, so a
    // 700-board load creates a handful of GPU buffers, not 1400.
    const placements = this.plan?.placements ?? [];
    placements.forEach((p, idx) => {
      const l = p.length_in * s;
      const w = p.width_in * s;
      const h = p.height_in * s;
      const geoKey = `${p.length_in}x${p.width_in}x${p.height_in}`;
      let geo = this._geoCache.get(geoKey);
      if (!geo) {
        const box = new THREE.BoxGeometry(l, h, w);
        geo = { box, edges: new THREE.EdgesGeometry(box) };
        this._geoCache.set(geoKey, geo);
      }

      let color: number;
      let edgeColor = 0x0a0b10;
      let edgeOpacity = 0.4;
      if (this.colorMode === 'wood') {
        color = jitterColor(woodToneFor(p.sku), idx);
        if (p.stop_sequence && p.stop_sequence > 0) {
          edgeColor = STOP_COLORS[(p.stop_sequence - 1) % STOP_COLORS.length];
          edgeOpacity = 0.85;
        }
      } else {
        const colorIdx = (p.axle_group - 1 + AXLE_COLORS.length) % AXLE_COLORS.length;
        color = AXLE_COLORS[colorIdx < 0 ? 0 : colorIdx];
      }

      const matKey = `m${this.colorMode}-${color}`;
      let mat = this._matCache.get(matKey);
      if (!mat) {
        mat = new THREE.MeshStandardMaterial({
          color,
          transparent: true,
          opacity: this.colorMode === 'wood' ? 1.0 : 0.92,
          roughness: this.colorMode === 'wood' ? 0.85 : 0.6,
        });
        this._matCache.set(matKey, mat);
      }
      const edgeKey = `e${edgeColor}-${edgeOpacity}`;
      let edgeMat = this._matCache.get(edgeKey);
      if (!edgeMat) {
        edgeMat = new THREE.LineBasicMaterial({ color: edgeColor, transparent: true, opacity: edgeOpacity });
        this._matCache.set(edgeKey, edgeMat);
      }

      const mesh = new THREE.Mesh(geo.box, mat);
      // Solver gives corner coords; Three boxes are centered.
      mesh.position.set(
        offX + p.x * s + l / 2,
        p.z * s + h / 2,
        offZ + p.y * s + w / 2,
      );
      mesh.userData.built = true;
      mesh.userData.cached = true;
      this._scene!.add(mesh);

      const edges = new THREE.LineSegments(geo.edges, edgeMat);
      edges.position.copy(mesh.position);
      edges.userData.built = true;
      edges.userData.cached = true;
      this._scene!.add(edges);

      this._placed.push({ mesh, edges, step: p.step ?? 0, mat, edgeMat });
    });

    this._buildStraps(offX, bed);
    this._applyVisibility();
  }

  /**
   * Tie-down ribbons from the securement plan: a flat band over the top of the
   * load with drops down both sides at each strap position. Shown only when
   * the full load is visible (straps go on after packing).
   */
  private _buildStraps(offX: number, bed: BedDims) {
    this._strapGroup = undefined;
    const straps = this.plan?.securement?.straps ?? [];
    if (straps.length === 0 || !this._scene) return;

    const s = this._scale;
    const group = new THREE.Group();
    group.userData.built = true;
    const strapMat = new THREE.MeshStandardMaterial({ color: 0xf59e0b, roughness: 0.55 });
    const bandW = 4 * s; // 4" webbing
    const bandT = 0.35 * s;

    for (const st of straps) {
      const h = st.over_height_in * s;
      if (h <= 0) continue;
      const x = offX + st.position_in * s;

      const top = new THREE.Mesh(new THREE.BoxGeometry(bandW, bandT, bed.width_in * s + bandT * 2), strapMat);
      top.position.set(x, h + bandT / 2, 0);
      group.add(top);

      for (const side of [-1, 1]) {
        const drop = new THREE.Mesh(new THREE.BoxGeometry(bandW, h + bandT, bandT), strapMat);
        drop.position.set(x, (h + bandT) / 2, side * (bed.width_in / 2) * s + side * bandT / 2);
        group.add(drop);
      }
    }
    // Group-level material/geometry are disposed via the non-cached path.
    group.traverse((c) => { c.userData.built = true; });
    this._scene.add(group);
    this._strapGroup = group;
  }

  /**
   * Packing playback: show placements up to visibleSteps and glow the current
   * one. Pure visibility/material swaps — no scene rebuild.
   */
  private _applyVisibility() {
    const limit = this.visibleSteps;
    if (this._strapGroup) {
      const total = this._placed.length;
      this._strapGroup.visible = limit < 0 || limit >= total;
    }
    for (const p of this._placed) {
      const hidden = limit >= 0 && p.step > 0 && p.step > limit;
      const isCurrent = limit >= 0 && p.step > 0 && p.step === limit;
      p.mesh.visible = !hidden;
      p.edges.visible = !hidden;
      p.mesh.material = isCurrent ? this._highlightMat : p.mat;
      (p.edges as THREE.LineSegments).material = isCurrent ? this._highlightEdgeMat : p.edgeMat;
    }
  }

  render() {
    const empty = !this.plan || (this.plan.placements?.length ?? 0) === 0;
    return html`
      <div class="relative w-full h-[420px] rounded-xl overflow-hidden border border-white/5 bg-deep-space">
        <div id="three-host" class="absolute inset-0"></div>
        ${empty
          ? html`<div class="absolute inset-0 flex items-center justify-center pointer-events-none">
              <span class="text-sm text-zinc-500">Run a load optimization to render the 3D view</span>
            </div>`
          : null}
        <div class="absolute bottom-3 left-3 text-[11px] text-zinc-500 font-mono pointer-events-none">
          drag to orbit · scroll to zoom
        </div>
      </div>
    `;
  }
}
