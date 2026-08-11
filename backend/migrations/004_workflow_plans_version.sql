-- SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
-- SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

-- 004_workflow_plans_version.sql
-- Optimistic concurrency for the guided dispatch workflow.
--
-- Every workflow mutation is a read-modify-write of the whole plan document
-- (repo.Get -> mutate -> repo.Update). The ordinary customer flow puts two
-- actors on one plan at the same time -- a dispatcher rerouting while a yard
-- lead attaches proof and signs off -- and each net/http request is served on
-- its own goroutine, so last-writer-wins silently discarded the other change
-- even at INSTANCE_COUNT=1.
--
-- `version` is the concurrency token: writes are guarded with
--   UPDATE ... WHERE id = $1 AND version = <the value the writer read>
-- and bump it, so the loser of a race affects zero rows and is answered
-- 409 Conflict instead of overwriting.
--
-- Additive and reversible: a new column with a constant default (a metadata-
-- only rewrite on PostgreSQL 11+), no data backfill, no change to any existing
-- column, and every pre-existing row starts at version 1.
ALTER TABLE workflow_plans
    ADD COLUMN IF NOT EXISTS version INTEGER NOT NULL DEFAULT 1;
