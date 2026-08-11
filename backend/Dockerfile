# SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
# SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

# ============================================================================
# AI_LM Backend — Multi-stage build
#
# Produces a minimal Alpine runtime image with three static binaries:
#   ./main     — API server (port 8090)
#   ./migrate  — migration runner
#   ./seed     — demo fleet profiles + restricted points seeder
#
# The same image backs the API service AND the POST_DEPLOY migrate-and-seed
# job — the job overrides the entrypoint via run_command.
# ============================================================================

# ---------------------------------------------------------------------------
# Stage 1: Build
# ---------------------------------------------------------------------------
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src

# Copy go.mod and go.sum first for layer caching
COPY backend/go.mod backend/go.sum ./
RUN go mod download

# Copy the rest of the backend source
COPY backend/ .

# Build all three binaries (static, no cgo)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/main    ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/migrate ./cmd/migrate
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/seed    ./cmd/seed

# ---------------------------------------------------------------------------
# Stage 2: Runtime
# ---------------------------------------------------------------------------
FROM alpine:3.20

RUN apk --no-cache add ca-certificates curl

WORKDIR /app

# Copy binaries
COPY --from=builder /out/main    .
COPY --from=builder /out/migrate .
COPY --from=builder /out/seed    .

# Copy migrations (needed by the migrate binary at runtime)
COPY backend/migrations/ ./migrations/

EXPOSE 8090

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -f http://localhost:8090/healthz/live || exit 1

CMD ["./main"]
