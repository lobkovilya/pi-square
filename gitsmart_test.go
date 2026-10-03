//go:build linux

package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestGitGateway(t *testing.T) (*gateway, *recordingUpstream) {
	t.Helper()
	rec := &recordingUpstream{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.lastReq = r.Clone(r.Context())
		rec.lastRaw = body
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	t.Cleanup(rec.server.Close)
	base, _ := url.Parse(rec.server.URL)
	return &gateway{
		token:           "real-credential",
		gitClient:       rec.server.Client(),
		gitUpstreamBase: base,
	}, rec
}

func dispatchTestGit(gw *gateway, class policyClass, req *http.Request) (*http.Response, bool) {
	route, recognized := classifyGitRoute(req)
	var deny *denial
	if recognized {
		deny = authorizeGit(class, route.service)
	}
	return gw.dispatchGit(class, req, route, recognized, deny, nil, nil)
}

func TestGitSmartHTTPPolicy(t *testing.T) {
	cases := []struct {
		name            string
		class           policyClass
		method          string
		target          string
		wantStatus      int
		wantDenial      string
		wantGatewayAuth bool
		wantClientAuth  bool
	}{
		{"upload advertisement ro", classRO, http.MethodGet, "https://github.com/o/r.git/info/refs?service=git-upload-pack", 200, "", true, false},
		{"upload rpc ro", classRO, http.MethodPost, "https://github.com/o/r/git-upload-pack", 200, "", true, false},
		{"receive advertisement ro", classRO, http.MethodGet, "https://github.com/o/r.git/info/refs?service=git-receive-pack", 403, denyWriteRequiresPublish, false, false},
		{"receive rpc ro", classRO, http.MethodPost, "https://github.com/o/r.git/git-receive-pack", 403, denyWriteRequiresPublish, false, false},
		{"receive rpc w", classW, http.MethodPost, "https://github.com/o/r.git/git-receive-pack", 200, "", true, false},
		{"release path preserves client credentials", classW, http.MethodGet, "https://github.com/o/r/releases/download/v1/file", 200, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw, rec := newTestGitGateway(t)
			req := newRequest(t, tc.method, tc.target, "body", http.Header{
				"Authorization": {"Basic client-secret"},
				"Cookie":        {"session=secret"},
			})

			resp, _ := dispatchTestGit(gw, tc.class, req)
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Get("X-Pi-Square-Denial"); got != tc.wantDenial {
				t.Fatalf("denial = %q, want %q", got, tc.wantDenial)
			}
			if tc.wantGatewayAuth {
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:real-credential"))
				if got := rec.lastReq.Header.Get("Authorization"); got != want {
					t.Errorf("Authorization = %q, want %q", got, want)
				}
			} else if tc.wantClientAuth {
				if got := rec.lastReq.Header.Get("Authorization"); got != "Basic client-secret" {
					t.Errorf("client Authorization = %q", got)
				}
				if got := rec.lastReq.Header.Get("Cookie"); got != "session=secret" {
					t.Errorf("client Cookie = %q", got)
				}
			} else if rec.lastReq != nil && (rec.lastReq.Header.Get("Authorization") != "" || rec.lastReq.Header.Get("Cookie") != "") {
				t.Errorf("credentials reached GitHub upstream: %v", rec.lastReq.Header)
			}
		})
	}
}

func TestGitReceivePackAdvertisementDenial(t *testing.T) {
	gw, upstream := newTestGitGateway(t)
	req := newRequest(t, http.MethodGet, "https://github.com/owner/repo.git/info/refs?service=git-receive-pack", "", nil)

	resp, _ := dispatchTestGit(gw, classRO, req)
	payload, err := io.ReadAll(resp.Body)
	resp.Body.Close()

	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden || !bytes.Contains(payload, []byte("write_requires_publish: git push requires publish profile\n")) {
		t.Fatalf("denial status = %d, body = %q", resp.StatusCode, payload)
	}
	if upstream.lastReq != nil {
		t.Fatal("denied advertisement reached upstream")
	}
}

func TestGitPushStreamsBodyOverAPILimit(t *testing.T) {
	gw, rec := newTestGitGateway(t)
	body := bytes.Repeat([]byte("p"), maxRequestBody+4096)
	req := httptest.NewRequest(http.MethodPost, "https://github.com/owner/repo.git/git-receive-pack", bytes.NewReader(body))

	resp, _ := dispatchTestGit(gw, classW, req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !bytes.Equal(rec.lastRaw, body) {
		t.Fatalf("upstream received %d bytes, want %d", len(rec.lastRaw), len(body))
	}
}

func TestGitTunnelAnswersExpectContinue(t *testing.T) {
	gw, _ := newTestGitGateway(t)
	ca, err := newEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.leafFor(gitHost, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	serverRaw, clientRaw := net.Pipe()
	serverTLS := tls.Server(serverRaw, &tls.Config{Certificates: []tls.Certificate{leaf}})
	clientTLS := tls.Client(clientRaw, &tls.Config{InsecureSkipVerify: true}) // test-only ephemeral CA
	done := make(chan struct{})
	go func() {
		gw.serveGitTunnel(serverTLS, classW)
		close(done)
	}()
	if err := clientTLS.Handshake(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		clientTLS.Close()
		<-done
	}()

	_, err = io.WriteString(clientTLS, "POST /owner/repo.git/git-receive-pack HTTP/1.1\r\nHost: github.com\r\nContent-Length: 4\r\nExpect: 100-continue\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(clientTLS)
	interim, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if interim.StatusCode != http.StatusContinue {
		t.Fatalf("interim status = %d", interim.StatusCode)
	}
	if _, err := io.WriteString(clientTLS, "push"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestGitEarlyUpstreamResponseClosesClientConnection(t *testing.T) {
	gw := &gateway{
		token:           "credential",
		gitUpstreamBase: &url.URL{Scheme: "https", Host: gitHost},
		gitClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusForbidden,
				Header:        http.Header{},
				Body:          io.NopCloser(strings.NewReader("rejected")),
				ContentLength: 8,
			}, nil
		})},
	}
	req := newRequest(t, http.MethodPost, "https://github.com/o/r.git/git-receive-pack", "pack data", nil)

	resp, keepAlive := dispatchTestGit(gw, classW, req)
	defer resp.Body.Close()

	if keepAlive {
		t.Fatal("connection remained reusable after an unconsumed request body")
	}
}

func TestGitDeniedExpectContinueRespondsWithoutReadingBody(t *testing.T) {
	gw, _ := newTestGitGateway(t)
	ca, err := newEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.leafFor(gitHost, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	serverRaw, clientRaw := net.Pipe()
	serverTLS := tls.Server(serverRaw, &tls.Config{Certificates: []tls.Certificate{leaf}})
	clientTLS := tls.Client(clientRaw, &tls.Config{InsecureSkipVerify: true}) // test-only ephemeral CA
	done := make(chan struct{})
	go func() {
		gw.serveGitTunnel(serverTLS, classRO)
		close(done)
	}()
	if err := clientTLS.Handshake(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		clientTLS.Close()
		<-done
	}()
	_ = clientTLS.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(clientTLS, "POST /owner/repo.git/git-receive-pack HTTP/1.1\r\nHost: github.com\r\nContent-Length: 500000000\r\nExpect: 100-continue\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden || !bytes.Contains(payload, []byte(denyWriteRequiresPublish)) {
		t.Fatalf("denial status = %d, body = %q", resp.StatusCode, payload)
	}
}

func TestGitClientDisconnectCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(started)
		<-req.Context().Done()
		close(canceled)
	}))
	upstream.Start()
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	gw := &gateway{token: "credential", gitClient: upstream.Client(), gitUpstreamBase: base}
	ca, err := newEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.leafFor(gitHost, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	serverRaw, clientRaw := net.Pipe()
	serverTLS := tls.Server(serverRaw, &tls.Config{Certificates: []tls.Certificate{leaf}})
	clientTLS := tls.Client(clientRaw, &tls.Config{InsecureSkipVerify: true}) // test-only ephemeral CA
	done := make(chan struct{})
	go func() {
		gw.serveGitTunnel(serverTLS, classRO)
		close(done)
	}()
	if err := clientTLS.Handshake(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(clientTLS, "GET /owner/repo.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: github.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	clientTLS.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel upstream request")
	}
	<-done
}

func TestSanitizeResponseHeadersStripsAuthenticationChallenge(t *testing.T) {
	header := http.Header{"WWW-Authenticate": {"Basic"}, "X-Keep": {"yes"}}
	sanitizeResponseHeaders(header, true)
	if header.Get("WWW-Authenticate") != "" || !strings.EqualFold(header.Get("X-Keep"), "yes") {
		t.Fatalf("sanitized headers = %v", header)
	}
}
