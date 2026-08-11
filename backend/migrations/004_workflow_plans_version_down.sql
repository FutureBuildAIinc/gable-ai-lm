-- SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
-- SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

-- Rollback of 004_workflow_plans_version.sql.
--
-- Dropping the column is lossless for plan data: `version` holds only a
-- monotonic write counter, never any dispatch information. After a rollback
-- the previous binary's unguarded `UPDATE ... WHERE id=$1` works again exactly
-- as before (it does not reference the column), so the pair is safe to apply
-- and revert in either order relative to a deploy.
ALTER TABLE workflow_plans
    DROP COLUMN IF EXISTS version;
