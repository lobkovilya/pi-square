//go:build linux

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type recordingUpstream struct {
	server  *httptest.Server
	lastReq *http.Request
	lastRaw []byte
}

func newTestGateway(t *testing.T) (*gateway, *recordingUpstream) {
	t.Helper()
	rec := &recordingUpstream{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.lastReq = r.Clone(r.Context())
		rec.lastRaw = body
		w.Header().Set("Set-Cookie", "session=secret")
		w.Header().Set("X-Upstream", "ok")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(rec.server.Close)

	base, _ := url.Parse(rec.server.URL)
	gw := &gateway{
		token:        "real-credential",
		client:       rec.server.Client(),
		upstreamBase: base,
	}
	return gw, rec
}

func newRequest(t *testing.T, method, target, body string, header http.Header) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if header != nil {
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	return req
}

func TestDispatchPolicyMatrix(t *testing.T) {
	jsonHeader := http.Header{"Content-Type": {"application/json"}}
	query := `{"query":"query { viewer { login } }"}`
	mutation := `{"query":"mutation { __typename }"}`

	cases := []struct {
		name       string
		class      policyClass
		method     string
		target     string
		body       string
		header     http.Header
		wantStatus int
		wantDenial string
	}{
		{"rest get ro", classRO, "GET", "https://api.github.com/rate_limit", "", nil, 200, ""},
		{"rest head ro", classRO, "HEAD", "https://api.github.com/rate_limit", "", nil, 200, ""},
		{"rest post ro", classRO, "POST", "https://api.github.com/user/repos", "{}", jsonHeader, 403, denyWriteRequiresPublish},
		{"rest put ro", classRO, "PUT", "https://api.github.com/x", "{}", jsonHeader, 403, denyWriteRequiresPublish},
		{"rest patch ro", classRO, "PATCH", "https://api.github.com/x", "{}", jsonHeader, 403, denyWriteRequiresPublish},
		{"rest delete ro", classRO, "DELETE", "https://api.github.com/x", "", nil, 403, denyWriteRequiresPublish},
		{"rest post w", classW, "POST", "https://api.github.com/user/repos", "{}", jsonHeader, 200, ""},
		{"rest delete w", classW, "DELETE", "https://api.github.com/x", "", nil, 200, ""},
		{"rest options", classW, "OPTIONS", "https://api.github.com/x", "", nil, 405, denyUnsupportedMethod},
		{"graphql query ro", classRO, "POST", "https://api.github.com/graphql", query, jsonHeader, 200, ""},
		{"graphql mutation ro", classRO, "POST", "https://api.github.com/graphql", mutation, jsonHeader, 403, denyWriteRequiresPublish},
		{"graphql mutation w", classW, "POST", "https://api.github.com/graphql", mutation, jsonHeader, 200, ""},
		{"graphql get", classRO, "GET", "https://api.github.com/graphql?query=%7Bviewer%7D", "", nil, 405, denyUnsupportedRequest},
		{"graphql wrong content type", classRO, "POST", "https://api.github.com/graphql", query, http.Header{"Content-Type": {"text/plain"}}, 415, denyUnsupportedRequest},
		{"host mismatch", classRO, "GET", "https://evil.example/x", "", nil, 403, denyUnsupportedHost},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw, _ := newTestGateway(t)
			resp, _ := gw.dispatch(tc.class, newRequest(t, tc.method, tc.target, tc.body, tc.header))
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			denial := resp.Header.Get("X-Pi-Square-Denial")
			if denial != tc.wantDenial {
				t.Fatalf("denial = %q, want %q", denial, tc.wantDenial)
			}
		})
	}
}

func TestForwardReplacesCredentialAndStripsHeaders(t *testing.T) {
	gw, rec := newTestGateway(t)
	header := http.Header{
		"Authorization": {"Bearer client-supplied"},
		"Cookie":        {"a=b"},
		"Connection":    {"X-Custom"},
		"X-Custom":      {"drop-me"},
		"Accept":        {"application/vnd.github+json"},
	}
	resp, keepAlive := gw.dispatch(classRO, newRequest(t, "GET", "https://api.github.com/rate_limit", "", header))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !keepAlive {
		t.Errorf("expected keep-alive for an allowed request")
	}
	if got := rec.lastReq.Header.Get("Authorization"); got != "Bearer real-credential" {
		t.Errorf("upstream Authorization = %q, want the gateway credential", got)
	}
	if got := rec.lastReq.Header.Get("Cookie"); got != "" {
		t.Errorf("Cookie should be stripped, got %q", got)
	}
	if got := rec.lastReq.Header.Get("X-Custom"); got != "" {
		t.Errorf("Connection-listed header should be stripped, got %q", got)
	}
	if got := rec.lastReq.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept should be preserved, got %q", got)
	}
	sanitizeResponseHeaders(resp.Header)
	if got := resp.Header.Get("Set-Cookie"); got != "" {
		t.Errorf("response Set-Cookie should be stripped, got %q", got)
	}
	if got := resp.Header.Get("X-Upstream"); got != "ok" {
		t.Errorf("benign response header should be preserved, got %q", got)
	}
}

func TestDispatchBlocksUpgrade(t *testing.T) {
	gw, _ := newTestGateway(t)
	header := http.Header{"Upgrade": {"websocket"}}
	resp, _ := gw.dispatch(classRO, newRequest(t, "GET", "https://api.github.com/x", "", header))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
