-- SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
-- SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

-- 003_workflow_plans.sql
-- The guided end-to-end dispatch workflow: one row per workflow run carrying
-- the full plan document (ingested order analyses, truck assignments, packed
-- load plans, compliance reviews, manifests) as a single JSONB payload that
-- advances through ANALYZED → ASSIGNED → PACKED → REVIEWED → PUSHED.
CREATE TABLE workflow_plans (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    plan_date DATE NOT NULL,
    status VARCHAR(30) NOT NULL DEFAULT 'ANALYZED',
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_workflow_plans_date ON workflow_plans(plan_date, created_at DESC);
