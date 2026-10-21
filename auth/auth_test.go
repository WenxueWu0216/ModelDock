package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wenxuewu0216/modeldock/domain"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func newAuth(t *testing.T) *Authenticator {
	t.Helper()
	a, err := NewAuthenticator(testSecret, "modeldock", "modeldock-api")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRBACMatrix(t *testing.T) {
	cases := []struct {
		role domain.Role
		want map[Permission]bool
	}{
		{domain.RoleViewer, map[Permission]bool{PermRead: true}},
		{domain.RoleDeployer, map[Permission]bool{PermRead: true, PermRegister: true, PermDeploy: true}},
		{domain.RoleApprover, map[Permission]bool{PermRead: true, PermApprove: true}},
		{domain.RoleAdmin, map[Permission]bool{PermRead: true, PermRegister: true, PermDeploy: true, PermApprove: true}},
		{domain.Role("intruder"), map[Permission]bool{}},
	}
	all := []Permission{PermRead, PermRegister, PermDeploy, PermApprove}
	for _, c := range cases {
		for _, p := range all {
			if got := Can(c.role, p); got != c.want[p] {
				t.Errorf("Can(%s, %s) = %v, want %v", c.role, p, got, c.want[p])
			}
		}
	}
}

func TestCanDecide(t *testing.T) {
	pending := domain.Approval{ID: "a1", RequestedBy: "dana", Status: domain.ApprovalPending}
	cases := []struct {
		name string
		user domain.User
		appr domain.Approval
		want error
	}{
		{"approver approves someone else's request", domain.User{ID: "alice", Role: domain.RoleApprover}, pending, nil},
		{"admin approves someone else's request", domain.User{ID: "root", Role: domain.RoleAdmin}, pending, nil},
		{"deployer cannot approve", domain.User{ID: "bob", Role: domain.RoleDeployer}, pending, domain.ErrForbidden},
		{"viewer cannot approve", domain.User{ID: "vic", Role: domain.RoleViewer}, pending, domain.ErrForbidden},
		{"requester cannot approve own request even as approver",
			domain.User{ID: "dana", Role: domain.RoleApprover}, pending, domain.ErrForbidden},
		{"admin cannot approve own request",
			domain.User{ID: "root", Role: domain.RoleAdmin},
			domain.Approval{ID: "a2", RequestedBy: "root", Status: domain.ApprovalPending}, domain.ErrForbidden},
		{"already decided",
			domain.User{ID: "alice", Role: domain.RoleApprover},
			domain.Approval{ID: "a3", RequestedBy: "dana", Status: domain.ApprovalApproved}, domain.ErrConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CanDecide(c.user, c.appr)
			if c.want == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	a := newAuth(t)
	tok, err := a.Issue(domain.User{ID: "alice", Name: "Alice", Role: domain.RoleApprover}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := a.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "alice" || u.Role != domain.RoleApprover || u.Name != "Alice" {
		t.Fatalf("got %+v", u)
	}
}

func TestVerifyRejects(t *testing.T) {
	a := newAuth(t)
	sign := func(c Claims, key []byte, m jwt.SigningMethod) string {
		s, err := jwt.NewWithClaims(m, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	valid := func() Claims {
		return Claims{Role: domain.RoleDeployer, RegisteredClaims: jwt.RegisteredClaims{
			Subject: "bob", Issuer: "modeldock", Audience: jwt.ClaimStrings{"modeldock-api"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		}}
	}
	expired := valid()
	expired.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	noExp := valid()
	noExp.ExpiresAt = nil
	badRole := valid()
	badRole.Role = "superuser"
	noSub := valid()
	noSub.Subject = ""
	wrongAud := valid()
	wrongAud.Audience = jwt.ClaimStrings{"other"}
	wrongIss := valid()
	wrongIss.Issuer = "evil"

	cases := map[string]string{
		"expired":     sign(expired, testSecret, jwt.SigningMethodHS256),
		"no exp":      sign(noExp, testSecret, jwt.SigningMethodHS256),
		"bad role":    sign(badRole, testSecret, jwt.SigningMethodHS256),
		"no subject":  sign(noSub, testSecret, jwt.SigningMethodHS256),
		"wrong aud":   sign(wrongAud, testSecret, jwt.SigningMethodHS256),
		"wrong iss":   sign(wrongIss, testSecret, jwt.SigningMethodHS256),
		"wrong key":   sign(valid(), []byte("ffffffffffffffffffffffffffffffff"), jwt.SigningMethodHS256),
		"alg HS512":   sign(valid(), testSecret, jwt.SigningMethodHS512),
		"garbage":     "not.a.jwt",
		"empty token": "",
	}
	// SigningMethodNone needs the special key; build it properly.
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, valid()).SignedString(jwt.UnsafeAllowNoneSignatureType)
	cases["alg none"] = none

	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := a.Verify(tok); !errors.Is(err, domain.ErrUnauthorized) {
				t.Fatalf("err = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestShortSecretRejected(t *testing.T) {
	if _, err := NewAuthenticator([]byte("short"), "", ""); err == nil {
		t.Fatal("expected error for short secret")
	}
}

func TestMiddleware(t *testing.T) {
	a := newAuth(t)
	var seen domain.User
	h := a.Middleware(func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, err.Error(), http.StatusUnauthorized)
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := CurrentUser(r.Context())
		if err != nil {
			t.Errorf("CurrentUser: %v", err)
		}
		seen = u
		w.WriteHeader(http.StatusNoContent)
	}))

	tok, _ := a.Issue(domain.User{ID: "dana", Role: domain.RoleDeployer}, time.Minute)
	cases := []struct {
		name, header string
		code         int
	}{
		{"valid", "Bearer " + tok, http.StatusNoContent},
		{"lowercase scheme", "bearer " + tok, http.StatusNoContent},
		{"missing", "", http.StatusUnauthorized},
		{"basic auth", "Basic Zm9vOmJhcg==", http.StatusUnauthorized},
		{"bearer no token", "Bearer ", http.StatusUnauthorized},
		{"tampered", "Bearer " + tok[:len(tok)-2] + "xx", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen = domain.User{}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.code {
				t.Fatalf("code = %d, want %d (%s)", rec.Code, c.code, strings.TrimSpace(rec.Body.String()))
			}
			if c.code == http.StatusNoContent && seen.ID != "dana" {
				t.Fatalf("user not propagated: %+v", seen)
			}
		})
	}
}

func TestCurrentUserWithoutMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := CurrentUser(req.Context()); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}
