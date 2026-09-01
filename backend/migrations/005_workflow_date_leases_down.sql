-- SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
-- SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

-- Rollback of 005_workflow_date_leases.sql.
--
-- Dropping the table is lossless for dispatch data: a row here records only
-- who is holding a date at this instant and until when, never anything about a
-- plan, a route or a truck. What it DOES do is remove the serialization: with
-- the table gone the previous binary is back to two writers racing on one
-- date, so this is a rollback to run with the binary that predates the lease,
-- not underneath the one that needs it.
DROP TABLE IF EXISTS workflow_date_leases;
