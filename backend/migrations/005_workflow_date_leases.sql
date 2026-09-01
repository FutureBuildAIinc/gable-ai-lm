-- SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
-- SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

-- 005_workflow_date_leases.sql
-- Exclusive, time-bounded hold on ONE dispatch date.
--
-- The date is the contended resource, not the plan. GableLBM's
-- ReplaceDeliveryRoute is keyed (vehicle_id, scheduled_date) and holds at most
-- one non-dispatched route per truck per day, so every path that writes this
-- dealer's dispatch board or the ledger mirroring it -- a push, a re-plan
-- (supersede), a re-assignment's recall -- has to be alone on that date while
-- it decides and writes.
--
-- WHY A ROW AND NOT pg_try_advisory_xact_lock
--
-- The advisory lock this replaces was TRANSACTION-scoped, which meant holding
-- the date required holding an open transaction, which meant holding one of
-- MaxConns=25 pooled connections for the whole operation -- across every
-- GableLBM round-trip inside it, up to one 15s ERP timeout per truck. Twenty-
-- five slow pushes on twenty-five DIFFERENT dates (which the lock deliberately
-- does not serialize against each other) therefore exhausted the pool for every
-- endpoint in the service, /health included. The lock was sound and its cost
-- was not survivable.
--
-- A lease row inverts that: the hold is DATA, taken in one statement and
-- released in one statement, and the connection goes back to the pool for the
-- whole of the work in between.
--
-- WHAT expires_at BUYS AND WHAT IT COSTS
--
-- An advisory transaction lock is released by COMMIT or ROLLBACK, so a process
-- killed mid-push could not wedge a date. A row survives its writer, so the
-- expiry is what replaces that guarantee: a crashed holder's date frees itself
-- within one lease TTL instead of never. The cost is that expiry is a clock and
-- not a fact, so the holder RENEWS while it works and stops the moment a
-- renewal finds the lease is no longer its own (internal/workflow/datelease.go).
--
-- holder is a per-acquisition token, not a process id: release and renewal are
-- both conditioned on it, so a holder whose lease expired and was taken by
-- somebody else cannot release or extend the new holder's lease.
CREATE TABLE IF NOT EXISTS workflow_date_leases (
    plan_date   DATE PRIMARY KEY,
    holder      TEXT NOT NULL,
    action      TEXT NOT NULL DEFAULT '',
    acquired_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Expired rows are reclaimed in place by the next acquirer's upsert, so no
-- sweeper is needed; this index is for the operator asking "what is held right
-- now, and by what", which is the first question during an incident.
CREATE INDEX IF NOT EXISTS idx_workflow_date_leases_expiry
    ON workflow_date_leases(expires_at);
