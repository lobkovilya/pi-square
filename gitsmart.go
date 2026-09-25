//go:build linux

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var gitSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type gitRoute struct {
	service       string
	advertisement bool
}

// classifyGitRoute recognizes only GitHub's smart HTTP discovery and RPC
// endpoints. Everything else on github.com is forwarded without a credential.
func classifyGitRoute(req *http.Request) (gitRoute, bool) {
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
	if len(parts) != 3 && len(parts) != 4 {
		return gitRoute{}, false
	}
	owner, repo := parts[0], parts[1]
	if strings.HasSuffix(repo, ".git") {
		repo = strings.TrimSuffix(repo, ".git")
	}
	if !validGitSegment(owner) || !validGitSegment(repo) {
		return gitRoute{}, false
	}

	if len(parts) == 3 && req.Method == http.MethodPost &&
		(parts[2] == gitUploadPack || parts[2] == gitReceivePack) && req.URL.RawQuery == "" {
		return gitRoute{service: parts[2]}, true
	}
	if len(parts) != 4 || req.Method != http.MethodGet || parts[2] != "info" || parts[3] != "refs" {
		return gitRoute{}, false
	}
	query := req.URL.Query()
	if len(query) != 1 || len(query["service"]) != 1 {
		return gitRoute{}, false
	}
	service := query.Get("service")
	if service != gitUploadPack && service != gitReceivePack {
		return gitRoute{}, false
	}
	return gitRoute{service: service, advertisement: true}, true
}

func validGitSegment(segment string) bool {
	return segment != "." && segment != ".." && gitSegmentPattern.MatchString(segment)
}

func (g *gateway) serveGitTunnel(tlsConn *tls.Conn, class policyClass) {
	br := bufio.NewReader(tlsConn)
	for {
		_ = tlsConn.SetReadDeadline(time.Now().Add(gatewayIdle))
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		route, recognized := classifyGitRoute(req)
		var deny *denial
		if recognized {
			deny = authorizeGit(class, route.service)
		}
		if strings.EqualFold(req.Header.Get("Expect"), "100-continue") && deny == nil {
			_ = tlsConn.SetWriteDeadline(time.Now().Add(gatewayResponse))
			if _, err := io.WriteString(tlsConn, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
				return
			}
			req.Header.Del("Expect")
		}

		resp, keepAlive := g.dispatchGit(class, req, route, recognized, deny, tlsConn, br)
		sanitizeResponseHeaders(resp.Header, recognized)
		resp.Close = !keepAlive
		resp.Body = &responseDeadlineBody{ReadCloser: resp.Body, conn: tlsConn}
		_ = tlsConn.SetWriteDeadline(time.Now().Add(gatewayResponse))
		err = resp.Write(tlsConn)
		_ = resp.Body.Close()
		if err != nil || !keepAlive {
			return
		}
	}
}

func (g *gateway) dispatchGit(class policyClass, req *http.Request, route gitRoute, recognized bool, deny *denial, conn *tls.Conn, br *bufio.Reader) (*http.Response, bool) {
	keepAlive := !req.Close
	service := "-"
	if recognized {
		service = route.service
	}
	if !hostMatches(req.Host, gitHost) {
		d := newDenial(denyUnsupportedHost, http.StatusForbidden, "unsupported request authority")
		g.logGit(class, req.Method, req.URL.Path, service, "deny:"+d.code, d.status)
		return gitDenialResponse(req, d), false
	}
	if req.Header.Get("Upgrade") != "" {
		d := newDenial(denyUnsupportedRequest, http.StatusBadRequest, "protocol upgrades are not supported")
		g.logGit(class, req.Method, req.URL.Path, service, "deny:"+d.code, d.status)
		return gitDenialResponse(req, d), false
	}
	if deny != nil {
		// Do not close an http.ReadRequest body here: Close drains it. In
		// particular, that deadlocks with a client waiting for 100 Continue.
		g.logGit(class, req.Method, req.URL.Path, service, "deny:"+deny.code, deny.status)
		return gitDenialResponse(req, deny), false
	}

	resp, bodyComplete, err := g.forwardGit(req, recognized, conn, br)
	if err != nil {
		g.logGit(class, req.Method, req.URL.Path, service, "upstream_error", http.StatusBadGateway)
		return gitDenialResponse(req, newDenial("upstream_error", http.StatusBadGateway, "upstream request failed")), false
	}
	// An upstream can reject a request before consuming its body. The
	// transport may still be draining from the buffered client connection, so
	// parsing another request would race with that drain and desynchronize it.
	keepAlive = keepAlive && bodyComplete
	g.logGit(class, req.Method, req.URL.Path, service, "allow", resp.StatusCode)
	return resp, keepAlive
}

func (g *gateway) forwardGit(req *http.Request, authenticated bool, conn *tls.Conn, br *bufio.Reader) (*http.Response, bool, error) {
	target := *g.gitUpstreamBase
	target.Path = req.URL.Path
	target.RawPath = req.URL.RawPath
	target.RawQuery = req.URL.RawQuery

	bodyComplete := true
	bodyDone := make(chan struct{})
	close(bodyDone)
	var deadlineConn netConnDeadline
	if conn != nil {
		deadlineConn = conn
	}
	var body io.Reader
	var tracked *requestDeadlineBody
	if req.Body != nil && req.Body != http.NoBody {
		tracked = newRequestDeadlineBody(req.Body, deadlineConn, req.ContentLength)
		body = tracked
		bodyDone = tracked.done
		bodyComplete = false
	}

	ctx, stopWatching := watchClientDisconnect(req.Context(), br, deadlineConn, bodyDone)
	upstream, err := http.NewRequestWithContext(ctx, req.Method, target.String(), body)
	if err != nil {
		stopWatching()
		return nil, false, err
	}
	upstream.Header = cloneForwardHeaders(req.Header, authenticated)
	upstream.Header.Del("Expect")
	upstream.Host = gitHost
	upstream.ContentLength = req.ContentLength
	if authenticated {
		credential := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.token))
		upstream.Header.Set("Authorization", "Basic "+credential)
	}

	resp, err := g.gitClient.Do(upstream)
	if tracked != nil {
		bodyComplete = tracked.complete.Load()
	}
	if err != nil {
		stopWatching()
		return nil, bodyComplete, err
	}
	resp.Body = &closeCallbackBody{ReadCloser: resp.Body, closeCallback: stopWatching}
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	if resp.ContentLength < 0 {
		resp.TransferEncoding = []string{"chunked"}
	}
	return resp, bodyComplete, nil
}

func gitDenialResponse(req *http.Request, d *denial) *http.Response {
	payload := []byte(d.code + ": " + d.message + "\n")
	header := http.Header{}
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("X-Pi-Square-Denial", d.code)
	return &http.Response{
		StatusCode:    d.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(string(payload))),
		ContentLength: int64(len(payload)),
		Request:       req,
	}
}

type requestDeadlineBody struct {
	io.ReadCloser
	conn      netConnDeadline
	done      chan struct{}
	doneOnce  sync.Once
	complete  atomic.Bool
	remaining int64
}

func newRequestDeadlineBody(body io.ReadCloser, conn netConnDeadline, contentLength int64) *requestDeadlineBody {
	return &requestDeadlineBody{ReadCloser: body, conn: conn, done: make(chan struct{}), remaining: contentLength}
}

func (b *requestDeadlineBody) Read(p []byte) (int, error) {
	if b.conn != nil {
		_ = b.conn.SetReadDeadline(time.Now().Add(gatewayResponse))
	}
	n, err := b.ReadCloser.Read(p)
	if b.remaining >= 0 {
		b.remaining -= int64(n)
	}
	if err == io.EOF || b.remaining == 0 {
		b.complete.Store(true)
		b.doneOnce.Do(func() { close(b.done) })
	}
	return n, err
}

type responseDeadlineBody struct {
	io.ReadCloser
	conn netConnDeadline
}

func (b *responseDeadlineBody) Read(p []byte) (int, error) {
	// Upstream wait time must not consume the deadline intended to bound the
	// subsequent client write.
	_ = b.conn.SetWriteDeadline(time.Time{})
	n, err := b.ReadCloser.Read(p)
	_ = b.conn.SetWriteDeadline(time.Now().Add(gatewayResponse))
	return n, err
}

type closeCallbackBody struct {
	io.ReadCloser
	closeCallback func()
	once          sync.Once
}

func (b *closeCallbackBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.closeCallback)
	return err
}

// watchClientDisconnect cancels the upstream request if the client closes
// after its request body is consumed. Peek preserves a pipelined next request;
// unlike a concurrent ReadRequest, it never races with body consumption.
func watchClientDisconnect(parent context.Context, br *bufio.Reader, conn netConnDeadline, bodyDone <-chan struct{}) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	if br == nil || conn == nil {
		return ctx, cancel
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-bodyDone:
		case <-stop:
			return
		}
		for {
			_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
			_, err := br.Peek(1)
			if err == nil {
				return
			}
			if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
				select {
				case <-stop:
					return
				default:
					continue
				}
			}
			cancel()
			return
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			close(stop)
			_ = conn.SetReadDeadline(time.Now())
			<-done
			_ = conn.SetReadDeadline(time.Time{})
			cancel()
		})
	}
}

type netConnDeadline interface {
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

func (g *gateway) logGit(class policyClass, method, path, service, outcome string, status int) {
	if g.logger != nil {
		g.logger.Printf("gateway class=%s method=%s path=%s service=%s result=%s status=%d", class, method, path, service, outcome, status)
	}
}
