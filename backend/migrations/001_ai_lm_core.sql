-- SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
-- SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

-- 001_ai_lm_core.sql
-- AI_LM core schema: fleet profiles, product dimensions, load plans,
-- restricted points, and route plans.
--
-- AI_LM is a standalone microservice. It does NOT own orders/products/vehicles —
-- those live in GableLBM and are pulled via /api/integration/*. The tables below
-- store ONLY the supplementary data the ERP lacks (axle layouts, bed dimensions,
-- product L/W/H) keyed by GableLBM UUIDs, plus AI_LM-native optimizer output.
-- This keeps the module portable to other ERPs for commercial licensing.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- ---------------------------------------------------------------------------
-- vehicle_profiles — per-truck supplementary attributes keyed by GableLBM
-- vehicle id. GableLBM has capacity_weight_lbs but no axle config or bed dims.
-- ---------------------------------------------------------------------------
CREATE TABLE vehicle_profiles (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    gable_vehicle_id    UUID NOT NULL UNIQUE,          -- link to GableLBM vehicle
    name                TEXT NOT NULL DEFAULT '',       -- cached display name
    bed_length_in       DECIMAL(19,4) NOT NULL DEFAULT 0,
    bed_width_in        DECIMAL(19,4) NOT NULL DEFAULT 0,
    bed_height_in       DECIMAL(19,4) NOT NULL DEFAULT 0,
    gvwr_lbs            BIGINT NOT NULL DEFAULT 0,       -- gross vehicle weight rating
    tare_weight_lbs     BIGINT NOT NULL DEFAULT 0,       -- empty/curb weight
    axle_layout         JSONB NOT NULL DEFAULT '{}'::jsonb, -- denormalized summary
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_vehicle_profiles_gable_vehicle_id ON vehicle_profiles (gable_vehicle_id);

-- ---------------------------------------------------------------------------
-- axles — child rows describing each axle's rating and position.
-- ---------------------------------------------------------------------------
CREATE TABLE axles (
    id                      UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    vehicle_profile_id      UUID NOT NULL REFERENCES vehicle_profiles(id) ON DELETE CASCADE,
    axle_number             INT NOT NULL,                   -- 1 = front-most
    max_weight_lbs          BIGINT NOT NULL DEFAULT 0,       -- legal/rated axle limit
    position_from_front_in  DECIMAL(19,4) NOT NULL DEFAULT 0,
    axle_type               TEXT NOT NULL DEFAULT 'DRIVE',   -- STEER/DRIVE/TRAILER/TAG
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE (vehicle_profile_id, axle_number)
);

CREATE INDEX idx_axles_vehicle_profile_id ON axles (vehicle_profile_id);

-- ---------------------------------------------------------------------------
-- product_dimensions — per-product L/W/H + stacking, keyed by GableLBM product
-- id. weight_lbs_override is nullable; when null, AI_LM uses the LBM weight_lbs.
-- ---------------------------------------------------------------------------
CREATE TABLE product_dimensions (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    gable_product_id    UUID NOT NULL UNIQUE,           -- link to GableLBM product
    sku                 TEXT NOT NULL DEFAULT '',        -- cached for display
    length_in           DECIMAL(19,4) NOT NULL DEFAULT 0,
    width_in            DECIMAL(19,4) NOT NULL DEFAULT 0,
    height_in           DECIMAL(19,4) NOT NULL DEFAULT 0,
    stackable           BOOLEAN NOT NULL DEFAULT TRUE,
    weight_lbs_override DECIMAL(19,4),                   -- nullable; else use LBM value
    default_source      TEXT NOT NULL DEFAULT 'UOM',     -- UOM/MANUAL/IMPORT
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_product_dimensions_gable_product_id ON product_dimensions (gable_product_id);

-- ---------------------------------------------------------------------------
-- load_plans — optimizer output for a single truck load. placements/axle_loads
-- are JSONB so the 3D view can render directly without a join explosion.
-- ---------------------------------------------------------------------------
CREATE TABLE load_plans (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    gable_route_id      UUID,                            -- optional source route
    gable_delivery_id   UUID,                            -- optional source stop
    gable_vehicle_id    UUID NOT NULL,
    placements          JSONB NOT NULL DEFAULT '[]'::jsonb, -- [{item_id,x,y,z,rot,dims,weight,axle_group}]
    total_weight_lbs    BIGINT NOT NULL DEFAULT 0,
    axle_loads          JSONB NOT NULL DEFAULT '[]'::jsonb, -- [{axle_number,weight_lbs,max_weight_lbs,status}]
    balance_score       REAL NOT NULL DEFAULT 0,         -- 0..1, higher = better balanced
    gvw_status          TEXT NOT NULL DEFAULT 'PASS',    -- PASS/WARN/FAIL
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_load_plans_gable_route_id ON load_plans (gable_route_id);
CREATE INDEX idx_load_plans_gable_vehicle_id ON load_plans (gable_vehicle_id);

-- ---------------------------------------------------------------------------
-- restricted_points — weight/height/width restricted bridges, overpasses, etc.
-- MVP uses lat/lng + haversine buffer (PostGIS optional later).
-- ---------------------------------------------------------------------------
CREATE TABLE restricted_points (
    id                   UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name                 TEXT NOT NULL,
    lat                  DOUBLE PRECISION NOT NULL,
    lng                  DOUBLE PRECISION NOT NULL,
    restriction_type     TEXT NOT NULL DEFAULT 'WEIGHT',  -- WEIGHT/HEIGHT/WIDTH/SEASONAL
    max_gross_weight_lbs BIGINT,                          -- nullable per type
    max_axle_weight_lbs  BIGINT,
    max_height_in        DECIMAL(19,4),
    notes                TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_restricted_points_latlng ON restricted_points (lat, lng);

-- ---------------------------------------------------------------------------
-- route_plans — cached optimizer output for a date/branch. stops is an ordered
-- JSONB list; status drives the dispatcher Approve → write-back flow.
-- ---------------------------------------------------------------------------
CREATE TABLE route_plans (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    plan_date           DATE NOT NULL,
    gable_branch_id     UUID,
    gable_vehicle_id    UUID,
    stops               JSONB NOT NULL DEFAULT '[]'::jsonb, -- [{order_id,sequence,lat,lng,address,weight_lbs}]
    total_distance_mi   DECIMAL(19,4) NOT NULL DEFAULT 0,
    total_duration_min  DECIMAL(19,4) NOT NULL DEFAULT 0,
    status              TEXT NOT NULL DEFAULT 'DRAFT',   -- DRAFT/APPROVED
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_route_plans_date ON route_plans (plan_date);
CREATE INDEX idx_route_plans_status ON route_plans (status);
