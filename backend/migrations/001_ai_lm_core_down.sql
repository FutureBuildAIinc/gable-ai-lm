-- SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
-- SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

-- 001_ai_lm_core_down.sql
-- Manual rollback for 001_ai_lm_core.sql. The migrator skips *_down.sql files;
-- run this explicitly with: psql -f migrations/001_ai_lm_core_down.sql
-- Drop in reverse FK dependency order.

DROP TABLE IF EXISTS route_plans;
DROP TABLE IF EXISTS restricted_points;
DROP TABLE IF EXISTS load_plans;
DROP TABLE IF EXISTS product_dimensions;
DROP TABLE IF EXISTS axles;
DROP TABLE IF EXISTS vehicle_profiles;

DELETE FROM schema_migrations WHERE version = '001_ai_lm_core.sql';
