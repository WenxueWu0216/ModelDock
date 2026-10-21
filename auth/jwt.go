package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wenxuewu0216/modeldock/domain"
)

// Claims are the JWT claims ModelDock understands. "sub" identifies the user
// and "role" carries exactly one RBAC role.
type Claims struct {
	Name string      `json:"name,omitempty"`
	Role domain.Role `json:"role"`
	jwt.RegisteredClaims
}

// Authenticator verifies HS256 tokens. Production deployments would usually
// verify RS256/ES256 tokens from an IdP via JWKS; the verification surface is
// the same, only the key function changes.
type Authenticator struct {
	secret   []byte
	issuer   string
	audience string
	now      func() time.Time
}

// NewAuthenticator builds an Authenticator. secret must be at least 32 bytes.
func NewAuthenticator(secret []byte, issuer, audience string) (*Authenticator, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("auth: JWT secret must be at least 32 bytes, got %d", len(secret))
	}
	return &Authenticator{secret: secret, issuer: issuer, audience: audience, now: time.Now}, nil
}

// Issue mints a token. It is used by the dev "token" subcommand and tests.
func (a *Authenticator) Issue(u domain.User, ttl time.Duration) (string, error) {
	now := a.now()
	c := Claims{
		Name: u.Name,
		Role: u.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID,
			Issuer:    a.issuer,
			Audience:  jwt.ClaimStrings{a.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(a.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return s, nil
}

// Verify parses and validates a token string and returns the user it names.
func (a *Authenticator) Verify(token string) (domain.User, error) {
	var c Claims
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(a.now),
	}
	if a.issuer != "" {
		opts = append(opts, jwt.WithIssuer(a.issuer))
	}
	if a.audience != "" {
		opts = append(opts, jwt.WithAudience(a.audience))
	}
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return a.secret, nil }, opts...)
	if err != nil {
		return domain.User{}, fmt.Errorf("%w: %v", domain.ErrUnauthorized, err)
	}
	if c.Subject == "" {
		return domain.User{}, fmt.Errorf("%w: token has no subject", domain.ErrUnauthorized)
	}
	if !c.Role.Valid() {
		return domain.User{}, fmt.Errorf("%w: token has unknown role %q", domain.ErrUnauthorized, c.Role)
	}
	return domain.User{ID: c.Subject, Name: c.Name, Role: c.Role}, nil
}

type ctxKey struct{}

// WithUser stores u in ctx.
func WithUser(ctx context.Context, u domain.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFrom returns the authenticated user stored in ctx.
func UserFrom(ctx context.Context) (domain.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(domain.User)
	return u, ok
}

// ErrorWriter renders an auth failure. The API package passes its JSON error
// writer so auth failures look like every other error.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, err error)

// Middleware rejects requests without a valid bearer token and stores the
// authenticated user in the request context.
func (a *Authenticator) Middleware(onErr ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			scheme, tok, ok := strings.Cut(h, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
				onErr(w, r, fmt.Errorf("%w: missing bearer token", domain.ErrUnauthorized))
				return
			}
			u, err := a.Verify(strings.TrimSpace(tok))
			if err != nil {
				onErr(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
		})
	}
}

// CurrentUser returns the authenticated user or an ErrUnauthorized error.
func CurrentUser(ctx context.Context) (domain.User, error) {
	u, ok := UserFrom(ctx)
	if !ok {
		return domain.User{}, fmt.Errorf("%w: no user in context", domain.ErrUnauthorized)
	}
	return u, nil
}
