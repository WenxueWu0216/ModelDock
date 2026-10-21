package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wenxuewu0216/modeldock/api"
	"github.com/wenxuewu0216/modeldock/auth"
	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/kube"
	"github.com/wenxuewu0216/modeldock/service"
	"github.com/wenxuewu0216/modeldock/store/storetest"
)

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c client) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (c client) expect(code int, method, path string, body any, out any) {
	c.t.Helper()
	got, b := c.do(method, path, body)
	if got != code {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, got, code, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("decode %s: %v", b, err)
		}
	}
}

func (c client) gql(query string, vars map[string]any) (data map[string]any, errs []map[string]any) {
	c.t.Helper()
	_, b := c.do("POST", "/graphql", map[string]any{"query": query, "variables": vars})
	var resp struct {
		Data   map[string]any   `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		c.t.Fatalf("decode %s: %v", b, err)
	}
	return resp.Data, resp.Errors
}

type harness struct {
	srv *httptest.Server
	rec *kube.Reconciler
	as  func(id string, role domain.Role) client
}

func newHarness(t *testing.T) harness {
	t.Helper()
	s := storetest.New(t)
	reg := prometheus.NewRegistry()
	svc := service.New(s, nil, service.NewMetrics(reg), false)
	rec := &kube.Reconciler{Store: s, Kube: kube.NewFake(), Transitions: svc, Timeout: time.Minute}
	svc.Kicker = rec
	authn, err := auth.NewAuthenticator([]byte("test-secret-test-secret-test-secret"), "modeldock", "modeldock-api")
	if err != nil {
		t.Fatal(err)
	}
	h := api.NewHandler(api.Config{Service: svc, Auth: authn, Metrics: api.NewHTTPMetrics(reg), Gatherer: reg,
		Ready: s.Ping, Playground: true})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return harness{srv: srv, rec: rec, as: func(id string, role domain.Role) client {
		tok, err := authn.Issue(domain.User{ID: id, Role: role}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return client{t: t, base: srv.URL, token: tok}
	}}
}

func (h harness) reconcile(t *testing.T) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if err := h.rec.ReconcileOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRESTLifecycle(t *testing.T) {
	h := newHarness(t)
	dep := h.as("dana", domain.RoleDeployer)
	appr := h.as("alice", domain.RoleApprover)
	view := h.as("vic", domain.RoleViewer)
	anon := client{t: t, base: h.srv.URL}

	anon.expect(401, "GET", "/api/v1/models", nil, nil)
	anon.expect(200, "GET", "/healthz", nil, nil)
	anon.expect(200, "GET", "/readyz", nil, nil)

	view.expect(403, "POST", "/api/v1/models", map[string]string{"name": "iris"}, nil)
	dep.expect(201, "POST", "/api/v1/models", map[string]string{"name": "iris", "description": "Iris classifier"}, nil)
	dep.expect(409, "POST", "/api/v1/models", map[string]string{"name": "iris"}, nil)
	dep.expect(400, "POST", "/api/v1/models", map[string]string{"name": "Not Valid"}, nil)
	dep.expect(400, "POST", "/api/v1/models", map[string]any{"name": "x", "unknown": 1}, nil)

	for _, v := range []string{"1.0.0", "1.1.0"} {
		dep.expect(201, "POST", "/api/v1/models/iris/versions", map[string]any{
			"version": v, "image": "ghcr.io/acme/iris:" + v,
			"metadata":  map[string]string{"accuracy": "0.9" + v[2:3]},
			"resources": map[string]string{"cpuRequest": "100m", "memoryRequest": "128Mi"},
			"scaling":   map[string]int{"minReplicas": 1, "maxReplicas": 3},
		}, nil)
	}
	dep.expect(404, "POST", "/api/v1/models/nope/versions", map[string]any{"version": "1", "image": "x:1"}, nil)

	var d domain.Deployment
	dep.expect(400, "POST", "/api/v1/models/iris/versions/1.0.0/deploy", map[string]string{"environment": "prod"}, nil)
	dep.expect(202, "POST", "/api/v1/models/iris/versions/1.0.0/deploy", map[string]string{"environment": "staging"}, &d)
	if d.Status != domain.DeployPending {
		t.Fatalf("deployment = %+v", d)
	}
	h.reconcile(t)
	view.expect(200, "GET", "/api/v1/deployments/"+d.ID, nil, &d)
	if d.Status != domain.DeployReady {
		t.Fatalf("status = %s", d.Status)
	}

	var a domain.Approval
	dep.expect(201, "POST", "/api/v1/models/iris/versions/1.0.0/promotion-requests", map[string]string{"comment": "ready"}, &a)
	dep.expect(403, "POST", "/api/v1/approvals/"+a.ID+"/approve", nil, nil) // deployer cannot approve own
	dep.expect(409, "POST", "/api/v1/models/iris/versions/1.0.0/promote", nil, nil)
	var pending []domain.Approval
	appr.expect(200, "GET", "/api/v1/approvals?status=pending", nil, &pending)
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	appr.expect(200, "POST", "/api/v1/approvals/"+a.ID+"/approve", map[string]string{"comment": "lgtm"}, &a)
	appr.expect(403, "POST", "/api/v1/models/iris/versions/1.0.0/promote", nil, nil) // approver cannot deploy
	dep.expect(202, "POST", "/api/v1/models/iris/versions/1.0.0/promote", nil, &d)
	h.reconcile(t)

	var st service.ModelStatus
	view.expect(200, "GET", "/api/v1/models/iris/status", nil, &st)
	if st.Current[domain.EnvProd] == nil || st.Current[domain.EnvProd].Status != domain.DeployReady {
		t.Fatalf("status = %+v", st)
	}

	var diff domain.VersionDiff
	view.expect(200, "GET", "/api/v1/models/iris/diff?from=1.0.0&to=1.1.0", nil, &diff)
	if len(diff.Changes) != 2 {
		t.Fatalf("diff = %+v", diff)
	}
	view.expect(400, "GET", "/api/v1/models/iris/diff?from=1.0.0", nil, nil)

	dep.expect(400, "POST", "/api/v1/models/iris/rollback", map[string]string{"environment": "qa"}, nil)
	dep.expect(409, "POST", "/api/v1/models/iris/rollback", map[string]string{"environment": "prod"}, nil)

	var hist []domain.Deployment
	view.expect(200, "GET", "/api/v1/models/iris/deployments?environment=staging", nil, &hist)
	if len(hist) != 1 {
		t.Fatalf("history = %+v", hist)
	}
	var audit []map[string]any
	view.expect(200, "GET", "/api/v1/audit?model=iris&limit=3", nil, &audit)
	if len(audit) != 3 || audit[0]["type"] != "deploy.ready" {
		t.Fatalf("audit = %+v", audit)
	}
	view.expect(404, "GET", "/api/v1/deployments/not-a-uuid", nil, nil)

	// Metrics are exposed, labelled by route.
	_, m := anon.do("GET", "/metrics", nil)
	for _, want := range []string{
		`modeldock_http_request_duration_seconds_bucket{code="202",method="POST",route="promote"`,
		`modeldock_promotions_total{model="iris"} 1`,
		`modeldock_deploy_duration_seconds_count{environment="prod",kind="promote",outcome="ready"} 1`,
	} {
		if !strings.Contains(string(m), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestGraphQL(t *testing.T) {
	h := newHarness(t)
	dep := h.as("dana", domain.RoleDeployer)
	appr := h.as("alice", domain.RoleApprover)

	_, errs := dep.gql(`mutation { createModel(name: "iris", description: "d") { id name } }`, nil)
	if errs != nil {
		t.Fatal(errs)
	}
	data, errs := dep.gql(`mutation($in: RegisterVersionInput!) { registerVersion(input: $in) { version state metadata { key value } scaling { minReplicas } } }`,
		map[string]any{"in": map[string]any{"model": "iris", "version": "1", "image": "ghcr.io/acme/iris:1",
			"metadata": []map[string]string{{"key": "framework", "value": "sklearn"}},
			"scaling":  map[string]int{"minReplicas": 2}}})
	if errs != nil {
		t.Fatal(errs)
	}
	rv := data["registerVersion"].(map[string]any)
	if rv["state"] != "registered" || rv["scaling"].(map[string]any)["minReplicas"].(float64) != 2 {
		t.Fatalf("registerVersion = %+v", rv)
	}

	_, errs = dep.gql(`mutation { deployToStaging(model: "iris", version: "1") { id status } }`, nil)
	if errs != nil {
		t.Fatal(errs)
	}
	h.reconcile(t)
	data, errs = dep.gql(`mutation { requestPromotion(model: "iris", version: "1") { id status } }`, nil)
	if errs != nil {
		t.Fatal(errs)
	}
	aid := data["requestPromotion"].(map[string]any)["id"].(string)

	// Self-approval surfaces as a typed GraphQL error.
	_, errs = dep.gql(`mutation($id: ID!) { approve(approvalId: $id) { status } }`, map[string]any{"id": aid})
	if len(errs) != 1 || errs[0]["extensions"].(map[string]any)["code"] != "FORBIDDEN" {
		t.Fatalf("errors = %+v", errs)
	}
	_, errs = appr.gql(`mutation($id: ID!) { approve(approvalId: $id, comment: "ok") { status decidedBy } }`, map[string]any{"id": aid})
	if errs != nil {
		t.Fatal(errs)
	}
	_, errs = dep.gql(`mutation { promote(model: "iris", version: "1") { revision environment } }`, nil)
	if errs != nil {
		t.Fatal(errs)
	}
	h.reconcile(t)

	// One round trip: model, versions, per-env status and history.
	data, errs = appr.gql(`{
		me { id role }
		model(name: "iris") {
			name
			versions { version state }
			staging { status version }
			prod { status version revision kind }
			deployments(limit: 5) { environment status }
		}
		missing: model(name: "nope") { name }
		audit(model: "iris", limit: 2) { type data }
	}`, nil)
	if errs != nil {
		t.Fatal(errs)
	}
	if data["me"].(map[string]any)["role"] != "approver" || data["missing"] != nil {
		t.Fatalf("data = %+v", data)
	}
	model := data["model"].(map[string]any)
	prod := model["prod"].(map[string]any)
	if prod["status"] != "ready" || prod["kind"] != "promote" {
		t.Fatalf("prod = %+v", prod)
	}
	if model["versions"].([]any)[0].(map[string]any)["state"] != "production" {
		t.Fatalf("versions = %+v", model["versions"])
	}
	if len(model["deployments"].([]any)) != 2 {
		t.Fatalf("deployments = %+v", model["deployments"])
	}

	// Unauthenticated GraphQL is rejected at the HTTP layer.
	anon := client{t: t, base: h.srv.URL}
	if code, _ := anon.do("POST", "/graphql", map[string]string{"query": "{ me { id } }"}); code != 401 {
		t.Fatalf("anon graphql = %d", code)
	}
	// Viewer cannot mutate.
	view := h.as("vic", domain.RoleViewer)
	_, errs = view.gql(`mutation { createModel(name: "x") { id } }`, nil)
	if len(errs) != 1 || errs[0]["extensions"].(map[string]any)["code"] != "FORBIDDEN" {
		t.Fatalf("errors = %+v", errs)
	}
	// Invalid query keeps the parser message.
	_, errs = view.gql(`{ nope }`, nil)
	if len(errs) == 0 || !strings.Contains(errs[0]["message"].(string), "nope") {
		t.Fatalf("errors = %+v", errs)
	}
}
