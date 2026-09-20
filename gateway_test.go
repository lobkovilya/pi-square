//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
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

func TestDenialIncludesGitHubCompatibleMessage(t *testing.T) {
	for _, target := range []string{"https://api.github.com/user/repos", "https://api.github.com/graphql"} {
		t.Run(target, func(t *testing.T) {
			gw, upstream := newTestGateway(t)
			req := newRequest(t, "POST", target, `{"query":"mutation { __typename }"}`, http.Header{"Content-Type": {"application/json"}})
			resp, _ := gw.dispatch(classRO, req)
			defer resp.Body.Close()
			var payload struct {
				Message string `json:"message"`
				Error   struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Pi-Square-Denial") != denyWriteRequiresPublish {
				t.Fatalf("unexpected denial: %d %v", resp.StatusCode, resp.Header)
			}
			if payload.Error.Code != denyWriteRequiresPublish || payload.Error.Message == "" || payload.Message != payload.Error.Code+": "+payload.Error.Message {
				t.Fatalf("missing client-visible denial reason: %+v", payload)
			}
			if upstream.lastReq != nil {
				t.Fatal("denied request reached upstream")
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

func TestNormalizeConnectAuthority(t *testing.T) {
	for _, tc := range []struct {
		authority string
		host      string
		port      string
		wantErr   bool
	}{
		{"API.GitHub.com.:443", "api.github.com", "443", false},
		{"example.com:0443", "example.com", "443", false},
		{"[2606:4700:4700::1111]:443", "2606:4700:4700::1111", "443", false},
		{"api.github.com", "", "", true},
		{"example.com:0", "", "", true},
		{"user@example.com:443", "", "", true},
	} {
		host, port, err := normalizeConnectAuthority(tc.authority)
		if (err != nil) != tc.wantErr || host != tc.host || port != tc.port {
			t.Errorf("normalizeConnectAuthority(%q) = %q, %q, %v", tc.authority, host, port, err)
		}
	}
}

func TestResolvePublicRejectsMixedAndLocalAnswers(t *testing.T) {
	gw := &gateway{lookupIP: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")}, nil
	}}
	if _, err := gw.resolvePublic(context.Background(), "example.com"); !errors.Is(err, errNonPublicDestination) {
		t.Fatalf("mixed DNS answer error = %v, want non-public destination", err)
	}
	for _, host := range []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "::1", "2001:db8::1"} {
		if _, err := gw.resolvePublic(context.Background(), host); !errors.Is(err, errNonPublicDestination) {
			t.Errorf("resolvePublic(%q) error = %v, want non-public destination", host, err)
		}
	}
}

func TestPassthroughBothClassesAndBufferedBytes(t *testing.T) {
	for _, class := range []policyClass{classRO, classW} {
		t.Run(string(class), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			upstreamDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					upstreamDone <- err
					return
				}
				defer conn.Close()
				payload := make([]byte, 5)
				if _, err := io.ReadFull(conn, payload); err == nil && string(payload) != "hello" {
					err = fmt.Errorf("payload = %q", payload)
				}
				if err == nil {
					_, err = io.WriteString(conn, "world")
				}
				upstreamDone <- err
			}()

			var logs bytes.Buffer
			gw := &gateway{
				token:  "must-not-be-sent",
				logger: log.New(&logs, "", 0),
				lookupIP: func(context.Context, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
				},
				dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
				},
			}
			server, client := net.Pipe()
			gatewayDone := make(chan struct{})
			go func() {
				gw.serveIngress(server, class)
				close(gatewayDone)
			}()

			// Include tunnel data in the CONNECT write to exercise parser read-ahead.
			io.WriteString(client, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\nhello")
			br := bufio.NewReader(client)
			resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT status = %d", resp.StatusCode)
			}
			got := make([]byte, 5)
			if _, err := io.ReadFull(br, got); err != nil || string(got) != "world" {
				t.Fatalf("tunnel response = %q, %v", got, err)
			}
			client.Close()
			select {
			case err := <-upstreamDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("upstream relay did not finish")
			}
			select {
			case <-gatewayDone:
			case <-time.After(time.Second):
				t.Fatal("gateway relay did not clean up")
			}
			text := logs.String()
			for _, want := range []string{"class=" + string(class), "destination=example.com:443", "connected_ip=93.184.216.34", "outcome=complete", "bytes_client_to_upstream=5", "bytes_upstream_to_client=5"} {
				if !strings.Contains(text, want) {
					t.Errorf("tunnel log missing %q: %s", want, text)
				}
			}
		})
	}
}
