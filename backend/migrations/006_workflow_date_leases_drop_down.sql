-- SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
-- SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

-- Rollback of 006_workflow_date_leases_drop.sql: put 005's table back.
--
-- Run this to go BACK to a binary that predates the session-lock hold. Under
-- the current binary the table is simply unused -- the hold is a session-scoped
-- advisory lock and nothing reads or writes these rows -- so recreating it
-- while the current binary is running restores a table, not a mechanism.
CREATE TABLE IF NOT EXISTS workflow_date_leases (
    plan_date   DATE PRIMARY KEY,
    holder      TEXT NOT NULL,
    action      TEXT NOT NULL DEFAULT '',
    acquired_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_workflow_date_leases_expiry
    ON workflow_date_leases(expires_at);
