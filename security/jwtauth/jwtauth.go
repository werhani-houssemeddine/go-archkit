// Package jwtauth implements app.TokenService with JSON Web Tokens
// (github.com/golang-jwt/jwt/v5).
//
// Security properties: the signing method is pinned on verification (no "alg: none" or
// algorithm-confusion), issuer and audience are enforced when configured, expired vs.
// invalid tokens yield distinct errors, HMAC secrets shorter than 32 bytes are refused,
// and Refresh verifies a token before re-issuing it.
//
// Prefer short-lived access tokens plus opaque, rotated, server-side refresh tokens
// (security.OpaqueToken) over long-lived JWT refresh tokens.
package jwtauth

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/ids"
)

// Options configure a Service. Provide Secret (HS256/384/512) or PrivateKey/PublicKey
// (RS*, PS*, ES*, EdDSA). A Service with only PublicKey can verify but not issue.
type Options struct {
	Method     jwt.SigningMethod // default HS256 with Secret
	Secret     []byte
	PrivateKey crypto.PrivateKey
	PublicKey  crypto.PublicKey
	Issuer     string
	Audience   []string
	DefaultTTL time.Duration // default 15m
	Leeway     time.Duration // clock skew tolerance, default 5s
	Now        func() time.Time
}

// Service issues and verifies JWTs.
type Service struct {
	o         Options
	signKey   any
	verifyKey any
}

var _ app.TokenService = (*Service)(nil)

// New validates options.
func New(o Options) (*Service, error) {
	s := &Service{o: o}
	if o.Method == nil {
		o.Method = jwt.SigningMethodHS256
		s.o.Method = o.Method
	}
	if _, isHMAC := o.Method.(*jwt.SigningMethodHMAC); isHMAC {
		if len(o.Secret) < 32 {
			return nil, errors.New("jwtauth: HMAC secret must be at least 32 bytes")
		}
		s.signKey, s.verifyKey = o.Secret, o.Secret
	} else {
		if o.PublicKey == nil {
			return nil, errors.New("jwtauth: PublicKey is required for asymmetric methods")
		}
		s.signKey, s.verifyKey = o.PrivateKey, o.PublicKey
	}
	if s.o.DefaultTTL <= 0 {
		s.o.DefaultTTL = 15 * time.Minute
	}
	if s.o.Leeway <= 0 {
		s.o.Leeway = 5 * time.Second
	}
	if s.o.Now == nil {
		s.o.Now = time.Now
	}
	return s, nil
}

type tokenClaims struct {
	jwt.RegisteredClaims
	Roles       []string       `json:"roles,omitempty"`
	Permissions []string       `json:"permissions,omitempty"`
	TenantID    string         `json:"tenant_id,omitempty"`
	Extra       map[string]any `json:"ext,omitempty"`
}

// Issue signs claims with ttl (0 = DefaultTTL). A unique token id (jti) is set.
func (s *Service) Issue(_ context.Context, c app.Claims, ttl time.Duration) (string, error) {
	if s.signKey == nil {
		return "", errors.New("jwtauth: verify-only service (no signing key)")
	}
	if c.Subject == "" {
		return "", errors.New("jwtauth: claims require a subject")
	}
	if ttl <= 0 {
		ttl = s.o.DefaultTTL
	}
	now := s.o.Now()
	tc := tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   c.Subject,
			Issuer:    s.o.Issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        ids.NewV7().String(),
		},
		Roles: c.Roles, Permissions: c.Permissions, TenantID: c.TenantID, Extra: c.Extra,
	}
	if len(s.o.Audience) > 0 {
		tc.Audience = s.o.Audience
	}
	return jwt.NewWithClaims(s.o.Method, tc).SignedString(s.signKey)
}

// Verify parses and validates a token ("Bearer " prefix tolerated).
func (s *Service) Verify(_ context.Context, token string) (app.Claims, error) {
	if len(token) > 7 && (token[:7] == "Bearer " || token[:7] == "bearer ") {
		token = token[7:]
	}
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{s.o.Method.Alg()}),
		jwt.WithLeeway(s.o.Leeway),
		jwt.WithTimeFunc(s.o.Now),
		jwt.WithExpirationRequired(),
	}
	if s.o.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(s.o.Issuer))
	}
	for _, a := range s.o.Audience {
		opts = append(opts, jwt.WithAudience(a))
	}
	var tc tokenClaims
	_, err := jwt.ParseWithClaims(token, &tc, func(*jwt.Token) (any, error) { return s.verifyKey, nil }, opts...)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return app.Claims{}, errs.ErrExpiredToken.WithCause(err)
		}
		return app.Claims{}, errs.ErrInvalidToken.WithCause(err)
	}
	if tc.Subject == "" {
		return app.Claims{}, errs.ErrInvalidToken.WithCause(fmt.Errorf("missing subject"))
	}
	c := app.Claims{Subject: tc.Subject, Roles: tc.Roles, Permissions: tc.Permissions, TenantID: tc.TenantID, Extra: tc.Extra, TokenID: tc.ID}
	if tc.IssuedAt != nil {
		c.IssuedAt = tc.IssuedAt.Time
	}
	if tc.ExpiresAt != nil {
		c.ExpiresAt = tc.ExpiresAt.Time
	}
	return c, nil
}

// Refresh verifies token, then issues a new one with the same claims and a fresh lifetime.
func (s *Service) Refresh(ctx context.Context, token string, ttl time.Duration) (string, error) {
	c, err := s.Verify(ctx, token)
	if err != nil {
		return "", err
	}
	return s.Issue(ctx, c, ttl)
}
