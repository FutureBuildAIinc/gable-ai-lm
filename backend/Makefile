# SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
# SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

.PHONY: run migrate seed build vet test tidy

run:
	go run ./cmd/server

migrate:
	go run ./cmd/migrate

seed:
	go run ./cmd/seed

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

tidy:
	go mod tidy
