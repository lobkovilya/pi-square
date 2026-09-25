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
	return gw.dispatchGit(class, req, route, recognized, deny, nil)
}

func TestGitSmartHTTPPolicy(t *testing.T) {
	cases := []struct {
		name       string
		class      policyClass
		method     string
		target     string
		wantStatus int
		wantDenial string
		wantAuth   bool
	}{
		{"upload advertisement ro", classRO, http.MethodGet, "https://github.com/o/r.git/info/refs?service=git-upload-pack", 200, "", true},
		{"upload rpc ro", classRO, http.MethodPost, "https://github.com/o/r/git-upload-pack", 200, "", true},
		{"receive advertisement ro", classRO, http.MethodGet, "https://github.com/o/r.git/info/refs?service=git-receive-pack", 200, denyWriteRequiresPublish, false},
		{"receive rpc ro", classRO, http.MethodPost, "https://github.com/o/r.git/git-receive-pack", 403, denyWriteRequiresPublish, false},
		{"receive rpc w", classW, http.MethodPost, "https://github.com/o/r.git/git-receive-pack", 200, "", true},
		{"release path is anonymous", classW, http.MethodGet, "https://github.com/o/r/releases/download/v1/file", 200, "", false},
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
			if tc.wantAuth {
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:real-credential"))
				if got := rec.lastReq.Header.Get("Authorization"); got != want {
					t.Errorf("Authorization = %q, want %q", got, want)
				}
			} else if rec.lastReq != nil && rec.lastReq.Header.Get("Authorization") != "" {
				t.Errorf("anonymous request forwarded Authorization %q", rec.lastReq.Header.Get("Authorization"))
			}
			if rec.lastReq != nil && rec.lastReq.Header.Get("Cookie") != "" {
				t.Error("Cookie reached GitHub upstream")
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
	if !bytes.Contains(payload, []byte("ERR write_requires_publish: git push requires publish mode\n")) {
		t.Fatalf("advertisement = %q", payload)
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

func TestSanitizeResponseHeadersStripsAuthenticationChallenge(t *testing.T) {
	header := http.Header{"WWW-Authenticate": {"Basic"}, "X-Keep": {"yes"}}
	sanitizeResponseHeaders(header)
	if header.Get("WWW-Authenticate") != "" || !strings.EqualFold(header.Get("X-Keep"), "yes") {
		t.Fatalf("sanitized headers = %v", header)
	}
}