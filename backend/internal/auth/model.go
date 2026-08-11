// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package auth implements AI_LM staff login (COMM-1 pillar 4). A staff member
// authenticates with their GableLBM email; AI_LM checks entitlement against
// GableLBM's /api/integration/validate-staff endpoint and, on success, mints a
// short-lived HMAC-signed AI_LM session JWT that pkg/middleware verifies with
// SESSION_SECRET. AI_LM never stores staff credentials — GableLBM remains the
// identity source of truth.
package auth

import "errors"

// SessionIssuer is stamped as the `iss` claim on AI_LM-minted session tokens so
// they are distinguishable from externally-issued (JWKS) tokens.
const SessionIssuer = "ai-lm"

// Sentinel errors mapped to HTTP status by the handler.
var (
	// ErrEmailRequired is returned when the login request omits an email.
	ErrEmailRequired = errors.New("email is required")
	// ErrNotEntitled is returned when GableLBM reports the staff member is not
	// entitled to AI_LM (maps to 403).
	ErrNotEntitled = errors.New("not authorized for AI_LM")
)

// LoginRequest is the POST /api/v1/auth/login body.
type LoginRequest struct {
	Email string `json:"email"`
}

// LoginResponse is returned on a successful login. Token is the AI_LM session
// JWT the frontend stores and replays as a Bearer token.
type LoginResponse struct {
	Token string   `json:"token"`
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}
