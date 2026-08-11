// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/middleware"

	"github.com/golang-jwt/jwt/v5"
)

// sessionTTL is how long a minted AI_LM session token stays valid.
const sessionTTL = 12 * time.Hour

// staffValidator is the GableLBM surface the login flow depends on
// (satisfied by *gable.Client).
type staffValidator interface {
	ValidateStaff(ctx context.Context, email string) (*gable.StaffValidation, error)
}

// Service validates staff against GableLBM and mints AI_LM session tokens.
type Service struct {
	gable         staffValidator
	sessionSecret []byte
}

// NewService builds the auth service. sessionSecret signs the session JWTs and
// must match the SESSION_SECRET the middleware verifies with.
func NewService(g staffValidator, sessionSecret string) *Service {
	return &Service{gable: g, sessionSecret: []byte(sessionSecret)}
}

// Login validates the email against GableLBM and, if the staff member is
// entitled to AI_LM, mints a session token carrying their identity and roles.
// Returns ErrEmailRequired or ErrNotEntitled for the client-facing cases.
func (s *Service) Login(ctx context.Context, email string) (*LoginResponse, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, ErrEmailRequired
	}

	v, err := s.gable.ValidateStaff(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("validate staff: %w", err)
	}
	if !v.Entitled {
		return nil, ErrNotEntitled
	}

	token, err := s.mintToken(v)
	if err != nil {
		return nil, fmt.Errorf("mint session token: %w", err)
	}
	return &LoginResponse{Token: token, Name: v.Name, Roles: v.Roles}, nil
}

// mintToken signs an HS256 AI_LM session JWT. Claims mirror UserClaims so the
// auth middleware populates the exact same identity (sub, email, roles) it does
// for externally-issued tokens.
func (s *Service) mintToken(v *gable.StaffValidation) (string, error) {
	now := time.Now()
	claims := middleware.UserClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   v.StaffID,
			Issuer:    SessionIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(sessionTTL)),
		},
		Email: v.Email,
		Roles: v.Roles,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(s.sessionSecret)
}
