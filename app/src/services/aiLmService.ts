// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * Typed client for the AI_LM backend (/api/v1/*). Mirrors the Go JSON shapes
 * in internal/{fleet,catalog,load,routing,compliance}. All HTTP goes through
 * fetchWithAuth so auth/timeout/retry behave consistently.
 */
import { fetchWithAuth } from './fetchClient.ts';

const BASE = '/api/v1';

// ---- shared error envelope (pkg/httputil) ------------------------------
interface ErrorEnvelope {
  error?: { code: string; message: string };
  meta?: { request_id: string };
}

async function jsonOrThrow<T>(res: Response): Promise<T> {
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    try {
      const body = (await res.json()) as ErrorEnvelope;
      if (body.error?.message) msg = body.error.message;
    } catch {
      /* non-JSON body */
    }
    throw new Error(msg);
  }
  return (await res.json()) as T;
}

// ---- fleet -------------------------------------------------------------
export interface Axle {
  id?: string;
  axle_number: number;
  max_weight_lbs: number;
  position_from_front_in: number;
  axle_type: string; // STEER/DRIVE/TRAILER/TAG
}

export interface VehicleProfile {
  id: string;
  gable_vehicle_id: string;
  name: string;
  bed_length_in: number;
  bed_width_in: number;
  bed_height_in: number;
  gvwr_lbs: number;
  tare_weight_lbs: number;
  axles: Axle[];
  created_at: string;
  updated_at: string;
}

export interface ProfileInput {
  name: string;
  bed_length_in: number;
  bed_width_in: number;
  bed_height_in: number;
  gvwr_lbs: number;
  tare_weight_lbs: number;
  axles: Omit<Axle, 'id'>[];
}

// ---- catalog -----------------------------------------------------------
export interface ProductDimension {
  id: string;
  gable_product_id: string;
  sku: string;
  length_in: number;
  width_in: number;
  height_in: number;
  stackable: boolean;
  weight_lbs_override?: number;
  default_source: string;
  created_at: string;
  updated_at: string;
}

// EffectiveProduct is the resolved load-planning view of a product: GableLBM
// PIM geometry merged with AI_LM overrides. geometry_source records the winning
// provider (OVERRIDE/PIM/FALLBACK) and has_geometry is false when no usable
// L/W/H exists so the Load Builder can flag the item.
export interface EffectiveProduct {
  gable_product_id: string;
  sku: string;
  name: string;
  category?: string;
  length_in: number;
  width_in: number;
  height_in: number;
  stackable: boolean;
  weight_lbs: number;
  geometry_source: 'OVERRIDE' | 'PIM' | 'FALLBACK';
  has_geometry: boolean;
}

// ---- load --------------------------------------------------------------
export interface LoadItem {
  product_id: string;
  sku: string;
  quantity: number;
  length_in: number;
  width_in: number;
  height_in: number;
  weight_lbs: number; // per-unit
  stackable: boolean;
}

export interface Placement {
  item_id: string;
  sku: string;
  x: number;
  y: number;
  z: number;
  length_in: number;
  width_in: number;
  height_in: number;
  weight_lbs: number;
  axle_group: number;
  // Sequenced (multi-stop) plans only:
  order_id?: string;
  stop_sequence?: number;
  step?: number; // 1-based physical packing order
}

// AxleLoad mirrors internal/load.AxleLoad.
//
// `status` is FOUR-valued, not three. UNKNOWN means the fleet profile never
// supplied a rating for this axle, so its load cannot be judged at all — it is
// NOT a degraded PASS, and `utilization` is 0 in that case purely because there
// is no denominator. Any UI that keys a colour map off `status` must handle
// UNKNOWN explicitly; falling through to the PASS branch paints an unjudgeable
// axle green (see load/model.go, "StatusUnknown is NOT a degraded PASS").
//
// `advisory` is always true from the solver: the per-axle split is a planning
// aid, not a certified scale ticket.
export interface AxleLoad {
  axle_number: number;
  weight_lbs: number;
  max_weight_lbs: number;
  utilization: number;
  status: 'PASS' | 'WARN' | 'FAIL' | 'UNKNOWN';
  // True for everything this solver produces: the per-axle split is a planning
  // estimate (steer axle at the bed origin, no overhang lever), not a scale
  // ticket. It is the flag the push gate reads — an advisory over-rating warns,
  // a certified one (advisory false/absent) blocks. Optional because an older
  // backend omits it, and absent must therefore mean "not claimed advisory".
  advisory?: boolean;
}

export interface Strap {
  number: number;
  position_in: number; // inches from the bed front
  over_height_in: number;
  required_wll_lbs: number;
}

export interface Securement {
  cargo_weight_lbs: number;
  min_aggregate_wll_lbs: number;
  straps: Strap[];
  recommended_strap: string;
  // Jurisdiction rule basis (T2-7).
  jurisdiction: string;
  ruleset_name: string;
  rule_basis: string;
  required_tie_downs: number;
  anchor_spacing_in?: number;
  notes: string[];
}

/**
 * Why the packer did not position an article — mirrors the reason codes in
 * backend/internal/load/unplaced.go.
 *
 * `NO_GEOMETRY` is an information gap: the SKU has no recorded L/W/H (joist
 * hangers, caulk, random-length linear-foot stock), so there is nothing to
 * place. The material still RIDES — the yard loads it by hand and it is simply
 * absent from the 3D twin. Every other reason is a capacity failure: the cargo
 * stays in the yard and the customer ships short.
 */
export type UnplacedReason =
  | 'NO_GEOMETRY'
  | 'TRUCK_FULL'
  | 'BED_VOLUME_FULL'
  | 'TOO_LARGE_FOR_BED'
  | 'NOT_STACKABLE'
  | (string & {}); // a reason a future backend adds — treated as blocking

export interface Unplaced {
  sku: string;
  quantity: number;
  reason: UnplacedReason;
  detail: string;
  weight_lbs?: number;
}

/**
 * Mirrors `Unplaced.Blocking()` in backend/internal/load/unplaced.go, including
 * its fail-closed default: everything except the one known-benign reason blocks,
 * so a reason added on the backend cannot slip through the UI as harmless.
 */
export function isBlockingUnplaced(u: Unplaced): boolean {
  return u.reason !== 'NO_GEOMETRY';
}

/** Renders one entry the way the backend's Unplaced.String() does. */
export function unplacedLabel(u: Unplaced): string {
  return `${u.sku} ×${u.quantity} (${u.detail})`;
}

export interface LoadPlan {
  id: string;
  gable_route_id?: string;
  gable_delivery_id?: string;
  gable_vehicle_id: string;
  placements: Placement[];
  total_weight_lbs: number;
  axle_loads: AxleLoad[];
  balance_score: number;
  // Overall weight verdict. Deliberately three-valued even though a per-axle
  // status can be UNKNOWN: load/model.go collapses UNKNOWN to FAIL here
  // ("blocking, and never PASS") and carries the reason on profile_status /
  // profile_issues below.
  gvw_status: 'PASS' | 'WARN' | 'FAIL';
  // Every article the packer did not position, each with a TYPED reason. The two
  // families are not interchangeable — see UnplacedReason. Never gate on
  // `unplaced.length`; branch on the reason (isBlockingUnplaced below).
  unplaced: Unplaced[];
  // Weight of cargo that RIDES but has no placement (the NO_GEOMETRY entries).
  // Already included in total_weight_lbs, and necessarily absent from
  // axle_loads because there is no position to attribute it to.
  unmodeled_weight_lbs?: number;
  max_load_height_in?: number;
  // Fleet-profile completeness (load/model.go Plan.ProfileStatus). INCOMPLETE
  // means a blank axle rating, a blank GVWR or no axles at all, and every weight
  // verdict on this plan is therefore untrustworthy — profile_issues names each
  // defect. Omitted by older backends, hence optional.
  profile_status?: 'COMPLETE' | 'INCOMPLETE';
  profile_issues?: string[];
  // Volume budget (T2-2).
  bed_volume_cuft?: number;
  usable_volume_cuft?: number;
  cargo_volume_cuft?: number;
  volume_utilization?: number;
  volume_status?: 'PASS' | 'WARN' | 'FAIL';
  securement?: Securement;
  created_at: string;
}

export interface OptimizeRequest {
  vehicle_id: string;
  route_id?: string;
  delivery_id?: string;
  items: LoadItem[];
}

// ---- routing -----------------------------------------------------------
export interface RouteStop {
  order_id: string;
  sequence: number;
  lat: number;
  lng: number;
  address?: string;
  weight_lbs: number;
}

export interface Load {
  vehicle_id: string;
  vehicle_name: string;
  driver_id: string;
  driver_name: string;
  capacity_weight_lbs: number;
  stops: RouteStop[];
  total_weight_lbs: number;
  total_distance_mi: number;
  total_duration_min: number;
}

export interface RoutePlan {
  id: string;
  plan_date: string;
  gable_branch_id?: string;
  gable_vehicle_id?: string;
  loads: Load[];
  unassigned_stops: RouteStop[];
  stops: RouteStop[];
  total_distance_mi: number;
  total_duration_min: number;
  status: 'DRAFT' | 'APPROVED';
  created_at: string;
  updated_at: string;
}

export interface PlanRequest {
  date: string;
  branch_id?: string;
  vehicle_id?: string;
  depot_lat?: number;
  depot_lng?: number;
}

// ---- compliance --------------------------------------------------------
export interface RestrictedPoint {
  id: string;
  name: string;
  lat: number;
  lng: number;
  restriction_type: 'WEIGHT' | 'HEIGHT' | 'WIDTH' | 'SEASONAL';
  max_gross_weight_lbs?: number;
  max_axle_weight_lbs?: number;
  max_height_in?: number;
  notes: string;
  created_at: string;
  updated_at: string;
}

export type RestrictedPointInput = Omit<RestrictedPoint, 'id' | 'created_at' | 'updated_at'>;

export interface RouteCheckRequest {
  route: { lat: number; lng: number }[];
  load: { gross_weight_lbs: number; max_axle_lbs: number; height_in: number };
  buffer_miles?: number;
}

export interface ComplianceFlag {
  point: RestrictedPoint;
  distance_mi: number;
  violation: string;
  severity: 'WARN' | 'FAIL';
}

export interface RouteCheckResult {
  status: 'PASS' | 'WARN' | 'FAIL';
  flags: ComplianceFlag[];
}

// ---- workflow (guided end-to-end dispatch) ------------------------------
export interface DimOverride {
  length_in: number;
  width_in: number;
  height_in: number;
  tolerance_pct?: number;
  source?: string; // MEASURED / AVERAGE
  note?: string;
}

export interface AnalyzedLine {
  product_id: string;
  sku: string;
  name?: string;
  quantity: number;
  unit_weight_lbs: number;
  unit_length_in: number;
  unit_width_in: number;
  unit_height_in: number;
  stackable: boolean;
  has_geometry: boolean;
  line_weight_lbs: number;
  line_volume_cuft: number;
  dim_override?: DimOverride;
}

export interface OrderAnalysis {
  order_id: string;
  customer_name?: string;
  address?: string;
  lat?: number;
  lng?: number;
  lines: AnalyzedLine[];
  total_weight_lbs: number;
  total_volume_cuft: number;
  max_length_in: number;
  piece_count: number;
  shape_profile: 'LONG_LOAD' | 'COMPACT' | 'MIXED';
  routable: boolean;
  priority: boolean;
  issues: string[];
}

export interface WorkflowStop {
  order_id: string;
  sequence: number;
  lat: number;
  lng: number;
  address?: string;
  customer_name?: string;
  weight_lbs: number;
  priority: boolean;
}

// AI dispatch briefing (LLM-generated). When AI is unconfigured `available` is
// false and `message` explains how to enable it.
export interface Briefing {
  available: boolean;
  model?: string;
  text?: string;
  message?: string;
}

export interface ComplianceAction {
  type: 'REROUTE' | 'LOAD_ADJUST' | 'MANUAL_REVIEW';
  description: string;
  resolved: boolean;
}

export interface ComplianceReview {
  status: 'PASS' | 'WARN' | 'FAIL';
  flags: ComplianceFlag[];
  actions: ComplianceAction[];
  // Capacity findings the dispatcher must SEE but may proceed past: an
  // over-rating on the advisory per-axle split, and lines with no digital-twin
  // geometry. They never block a push, so the UI is required to render them.
  advisories?: string[];
  detours?: { lat: number; lng: number }[];
  checked_gross_lbs: number;
  checked_max_axle_lbs: number;
  checked_height_in: number;
}

// Yard proof-of-load + sign-off (T1-6).
export interface ProofAttachment {
  url: string;
  kind: string; // PHOTO / VIDEO
  caption?: string;
  added_by?: string;
  added_at: string;
}

export interface LoadProof {
  attachments: ProofAttachment[];
  signed_off: boolean;
  signed_by?: string;
  signed_role?: string;
  signed_at?: string;
  note?: string;
}

export interface TruckLoad {
  vehicle_id: string;
  vehicle_name: string;
  driver_id?: string;
  driver_name?: string;
  capacity_weight_lbs: number;
  stops: WorkflowStop[];
  total_weight_lbs: number;
  total_distance_mi: number;
  total_duration_min: number;
  bed?: { length_in: number; width_in: number; height_in: number };
  load_plan?: LoadPlan;
  compliance?: ComplianceReview;
  proof?: LoadProof;
}

export type WorkflowStatus = 'ANALYZED' | 'ASSIGNED' | 'PACKED' | 'REVIEWED' | 'PUSHED';

// Scheduled re-optimization lock state (T2-3).
export interface PlanLock {
  locked: boolean;
  window?: 'MORNING' | 'AFTERNOON' | 'CUSTOM';
  lock_at?: string;
  locked_by?: string;
  locked_at?: string;
  reason?: string;
}

export interface LateAdd {
  order_id: string;
  customer_name?: string;
  status: 'PENDING' | 'APPROVED' | 'REJECTED';
  requested_by?: string;
  requested_at: string;
  resolved_by?: string;
  note?: string;
}

export interface WorkflowPlan {
  id: string;
  plan_date: string;
  status: WorkflowStatus;
  depot_lat: number;
  depot_lng: number;
  orders: OrderAnalysis[];
  loads: TruckLoad[];
  unassigned_orders: WorkflowStop[];
  lock?: PlanLock;
  late_adds?: LateAdd[];
  created_at: string;
  updated_at: string;
}

// ---- auth (staff login) ------------------------------------------------
export interface LoginResponse {
  token: string;
  name: string;
  roles: string[];
}

// ---- service singleton -------------------------------------------------
class AiLmService {
  // fleet
  listProfiles(): Promise<VehicleProfile[]> {
    return fetchWithAuth(`${BASE}/fleet/profiles`).then((r) => jsonOrThrow(r));
  }
  getProfile(vehicleId: string): Promise<VehicleProfile> {
    return fetchWithAuth(`${BASE}/fleet/profiles/${vehicleId}`).then((r) => jsonOrThrow(r));
  }
  upsertProfile(vehicleId: string, input: ProfileInput): Promise<VehicleProfile> {
    return fetchWithAuth(`${BASE}/fleet/profiles/${vehicleId}`, {
      method: 'PUT',
      body: JSON.stringify(input),
    }).then((r) => jsonOrThrow(r));
  }

  // catalog
  listDimensions(): Promise<ProductDimension[]> {
    return fetchWithAuth(`${BASE}/catalog/dimensions`).then((r) => jsonOrThrow(r));
  }
  listProducts(): Promise<EffectiveProduct[]> {
    return fetchWithAuth(`${BASE}/catalog/products`).then((r) => jsonOrThrow(r));
  }

  // load
  optimizeLoad(req: OptimizeRequest): Promise<LoadPlan> {
    return fetchWithAuth(`${BASE}/load/optimize`, {
      method: 'POST',
      body: JSON.stringify(req),
    }).then((r) => jsonOrThrow(r));
  }
  getLoadPlan(id: string): Promise<LoadPlan> {
    return fetchWithAuth(`${BASE}/load/${id}`).then((r) => jsonOrThrow(r));
  }

  // routing
  planRoute(req: PlanRequest): Promise<RoutePlan> {
    return fetchWithAuth(`${BASE}/routing/plan`, {
      method: 'POST',
      body: JSON.stringify(req),
    }).then((r) => jsonOrThrow(r));
  }
  getRoutePlan(id: string): Promise<RoutePlan> {
    return fetchWithAuth(`${BASE}/routing/plan/${id}`).then((r) => jsonOrThrow(r));
  }
  approveRoutePlan(id: string): Promise<RoutePlan> {
    return fetchWithAuth(`${BASE}/routing/plan/${id}/approve`, { method: 'POST' }).then((r) =>
      jsonOrThrow(r),
    );
  }

  // compliance
  listRestrictedPoints(): Promise<RestrictedPoint[]> {
    return fetchWithAuth(`${BASE}/compliance/restricted-points`).then((r) => jsonOrThrow(r));
  }
  checkRoute(req: RouteCheckRequest): Promise<RouteCheckResult> {
    return fetchWithAuth(`${BASE}/compliance/check-route`, {
      method: 'POST',
      body: JSON.stringify(req),
    }).then((r) => jsonOrThrow(r));
  }
  createRestrictedPoint(input: RestrictedPointInput): Promise<RestrictedPoint> {
    return fetchWithAuth(`${BASE}/compliance/restricted-points`, {
      method: 'POST',
      body: JSON.stringify(input),
    }).then((r) => jsonOrThrow(r));
  }

  // workflow (guided end-to-end dispatch)
  ingestWorkflow(date: string): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans`, {
      method: 'POST',
      body: JSON.stringify({ date }),
    }).then((r) => jsonOrThrow(r));
  }
  latestWorkflow(date: string): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/latest?date=${encodeURIComponent(date)}`).then(
      (r) => jsonOrThrow(r),
    );
  }
  getWorkflow(id: string): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}`).then((r) => jsonOrThrow(r));
  }
  // override authorizes a reshuffle on a locked run (T2-3).
  assignWorkflow(id: string, override = false, approvedBy = ''): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/assign`, {
      method: 'POST',
      body: JSON.stringify({ override, approved_by: approvedBy }),
    }).then((r) => jsonOrThrow(r));
  }
  packWorkflow(id: string): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/pack`, { method: 'POST' }).then((r) =>
      jsonOrThrow(r),
    );
  }
  resequenceWorkflow(
    id: string,
    vehicleId: string,
    orderIds: string[],
    override = false,
    approvedBy = '',
  ): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/loads/${vehicleId}/sequence`, {
      method: 'PUT',
      body: JSON.stringify({ order_ids: orderIds, override, approved_by: approvedBy }),
    }).then((r) => jsonOrThrow(r));
  }
  setStopPriority(
    id: string,
    orderId: string,
    priority: boolean,
    override = false,
    approvedBy = '',
  ): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/stops/${orderId}/priority`, {
      method: 'PUT',
      body: JSON.stringify({ priority, override, approved_by: approvedBy }),
    }).then((r) => jsonOrThrow(r));
  }
  // T2-2: per-order dimension override for a variable-dimension SKU.
  setLineDimensions(
    id: string,
    orderId: string,
    body: { product_id?: string; sku?: string } & DimOverride,
  ): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/orders/${orderId}/dimensions`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }).then((r) => jsonOrThrow(r));
  }
  // T1-6: yard proof-of-load + sign-off.
  attachProof(
    id: string,
    vehicleId: string,
    body: { url: string; kind?: string; caption?: string; added_by?: string },
  ): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/loads/${vehicleId}/proof`, {
      method: 'POST',
      body: JSON.stringify(body),
    }).then((r) => jsonOrThrow(r));
  }
  signOffLoad(
    id: string,
    vehicleId: string,
    body: { signed_by: string; role?: string; note?: string },
  ): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/loads/${vehicleId}/sign-off`, {
      method: 'POST',
      body: JSON.stringify(body),
    }).then((r) => jsonOrThrow(r));
  }
  // T2-3: lock states + late-add escalation.
  lockPlan(
    id: string,
    body: { locked?: boolean; window?: string; lock_at?: string; reason?: string; locked_by?: string },
  ): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/lock`, {
      method: 'POST',
      body: JSON.stringify(body),
    }).then((r) => jsonOrThrow(r));
  }
  unlockPlan(id: string, reason = '', by = ''): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/unlock`, {
      method: 'POST',
      body: JSON.stringify({ reason, locked_by: by }),
    }).then((r) => jsonOrThrow(r));
  }
  addLateOrder(
    id: string,
    body: { order_id: string; requested_by?: string; note?: string },
  ): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/late-adds`, {
      method: 'POST',
      body: JSON.stringify(body),
    }).then((r) => jsonOrThrow(r));
  }
  resolveLateAdd(
    id: string,
    orderId: string,
    body: { reject?: boolean; approved_by?: string },
  ): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/late-adds/${orderId}/resolve`, {
      method: 'POST',
      body: JSON.stringify(body),
    }).then((r) => jsonOrThrow(r));
  }
  reviewWorkflow(id: string): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/review`, { method: 'POST' }).then((r) =>
      jsonOrThrow(r),
    );
  }
  getBriefing(id: string): Promise<Briefing> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/briefing`).then((r) => jsonOrThrow(r));
  }
  pushWorkflow(id: string): Promise<WorkflowPlan> {
    return fetchWithAuth(`${BASE}/workflow/plans/${id}/push`, { method: 'POST' }).then((r) =>
      jsonOrThrow(r),
    );
  }
  // auth (staff login backed by GableLBM validate-staff)
  login(email: string): Promise<LoginResponse> {
    return fetchWithAuth(`${BASE}/auth/login`, {
      method: 'POST',
      body: JSON.stringify({ email }),
    }).then((r) => jsonOrThrow(r));
  }
}

export const aiLmService = new AiLmService();
