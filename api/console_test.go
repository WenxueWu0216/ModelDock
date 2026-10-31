package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wenxuewu0216/modeldock/api"
	"github.com/wenxuewu0216/modeldock/auth"
)

func consoleServer(t *testing.T, cfg api.Config) *httptest.Server {
	t.Helper()
	authn, err := auth.NewAuthenticator([]byte("test-secret-test-secret-test-secret"), "modeldock", "modeldock-api")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth = authn
	srv := httptest.NewServer(api.NewHandler(cfg))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, method, url string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestConsoleServesSPA(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":           {Data: []byte("<html>console</html>")},
		"assets/index-abc.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc.css": {Data: []byte("body{}")},
	}
	srv := consoleServer(t, api.Config{Console: fsys})

	for _, p := range []string{"/", "/index.html", "/approvals", "/models/iris"} {
		resp, body := get(t, "GET", srv.URL+p)
		if resp.StatusCode != 200 || body != "<html>console</html>" || resp.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("GET %s: %d %q %v", p, resp.StatusCode, body, resp.Header)
		}
	}
	resp, body := get(t, "GET", srv.URL+"/assets/index-abc.js")
	if resp.StatusCode != 200 || body != "console.log(1)" || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, _ := get(t, "GET", srv.URL+"/assets/missing.js"); resp.StatusCode != 404 {
		t.Errorf("missing asset: %d", resp.StatusCode)
	}
	if resp, _ := get(t, "POST", srv.URL+"/"); resp.StatusCode != 405 {
		t.Errorf("POST /: %d", resp.StatusCode)
	}
	// The API keeps precedence over the catch-all.
	if resp, body := get(t, "GET", srv.URL+"/api/v1/models"); resp.StatusCode != 401 || !strings.Contains(body, "error") {
		t.Errorf("API under console: %d %q", resp.StatusCode, body)
	}
	if resp, _ := get(t, "GET", srv.URL+"/healthz"); resp.StatusCode != 200 {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
}

func TestConsolePlaceholderWhenNotEmbedded(t *testing.T) {
	srv := consoleServer(t, api.Config{ServeConsole: true})
	resp, body := get(t, "GET", srv.URL+"/")
	if resp.StatusCode != 200 || !strings.Contains(body, "not embedded") {
		t.Fatalf("placeholder: %d %q", resp.StatusCode, body)
	}
	if resp, _ := get(t, "GET", srv.URL+"/whatever"); resp.StatusCode != 404 {
		t.Fatalf("unknown path: %d", resp.StatusCode)
	}
}
