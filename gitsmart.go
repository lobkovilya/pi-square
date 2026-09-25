//go:build linux

package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
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
		tlsConn.SetReadDeadline(time.Now().Add(gatewayIdle))
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		route, recognized := classifyGitRoute(req)
		deny := (*denial)(nil)
		if recognized {
			deny = authorizeGit(class, route.service)
		}
		if strings.EqualFold(req.Header.Get("Expect"), "100-continue") && deny == nil {
			tlsConn.SetWriteDeadline(time.Now().Add(gatewayResponse))
			if _, err := io.WriteString(tlsConn, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
				req.Body.Close()
				return
			}
			req.Header.Del("Expect")
		}

		resp, keepAlive := g.dispatchGit(class, req, route, recognized, deny, tlsConn)
		sanitizeResponseHeaders(resp.Header)
		resp.Close = !keepAlive
		resp.Body = &deadlineBody{ReadCloser: resp.Body, conn: tlsConn}
		tlsConn.SetWriteDeadline(time.Now().Add(gatewayResponse))
		err = resp.Write(tlsConn)
		resp.Body.Close()
		if err != nil || !keepAlive {
			return
		}
	}
}

func (g *gateway) dispatchGit(class policyClass, req *http.Request, route gitRoute, recognized bool, deny *denial, conn *tls.Conn) (*http.Response, bool) {
	keepAlive := !req.Close
	service := "-"
	if recognized {
		service = route.service
	}
	if !hostMatches(req.Host, gitHost) {
		req.Body.Close()
		d := newDenial(denyUnsupportedHost, http.StatusForbidden, "unsupported request authority")
		g.logGit(class, req.Method, req.URL.Path, service, "deny:"+d.code, d.status)
		return g.denialResponse(req, d), false
	}
	if req.Header.Get("Upgrade") != "" {
		req.Body.Close()
		d := newDenial(denyUnsupportedRequest, http.StatusBadRequest, "protocol upgrades are not supported")
		g.logGit(class, req.Method, req.URL.Path, service, "deny:"+d.code, d.status)
		return g.denialResponse(req, d), false
	}
	if deny != nil {
		req.Body.Close()
		g.logGit(class, req.Method, req.URL.Path, service, "deny:"+deny.code, deny.status)
		if route.advertisement && route.service == gitReceivePack {
			return gitAdvertisementDenial(req, deny), keepAlive
		}
		return g.denialResponse(req, deny), keepAlive
	}

	resp, err := g.forwardGit(req, recognized, conn)
	if err != nil {
		g.logGit(class, req.Method, req.URL.Path, service, "upstream_error", http.StatusBadGateway)
		return g.denialResponse(req, newDenial("upstream_error", http.StatusBadGateway, "upstream request failed")), false
	}
	g.logGit(class, req.Method, req.URL.Path, service, "allow", resp.StatusCode)
	return resp, keepAlive
}

func (g *gateway) forwardGit(req *http.Request, authenticated bool, conn *tls.Conn) (*http.Response, error) {
	target := *g.gitUpstreamBase
	target.Path = req.URL.Path
	target.RawPath = req.URL.RawPath
	target.RawQuery = req.URL.RawQuery

	var body io.Reader
	if req.Body != nil && req.Body != http.NoBody {
		body = req.Body
		if conn != nil {
			body = &deadlineBody{ReadCloser: req.Body, conn: conn}
		}
	}
	upstream, err := http.NewRequestWithContext(req.Context(), req.Method, target.String(), body)
	if err != nil {
		return nil, err
	}
	upstream.Header = cloneAllowedHeaders(req.Header)
	upstream.Header.Del("Expect")
	upstream.Host = gitHost
	upstream.ContentLength = req.ContentLength
	if authenticated {
		credential := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.token))
		upstream.Header.Set("Authorization", "Basic "+credential)
	}

	resp, err := g.gitClient.Do(upstream)
	if err != nil {
		return nil, err
	}
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	if resp.ContentLength < 0 {
		resp.TransferEncoding = []string{"chunked"}
	}
	return resp, nil
}

func gitAdvertisementDenial(req *http.Request, d *denial) *http.Response {
	message := "ERR " + d.code + ": " + d.message + "\n"
	payload := []byte(fmt.Sprintf("%04x%s", len(message)+4, message))
	header := http.Header{}
	header.Set("Content-Type", "application/x-git-receive-pack-advertisement")
	header.Set("X-Pi-Square-Denial", d.code)
	return &http.Response{
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(payload)),
		ContentLength: int64(len(payload)),
		Request:       req,
	}
}

type deadlineBody struct {
	io.ReadCloser
	conn netConnDeadline
}

type netConnDeadline interface {
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	_ = b.conn.SetReadDeadline(time.Now().Add(gatewayResponse))
	_ = b.conn.SetWriteDeadline(time.Now().Add(gatewayResponse))
	return b.ReadCloser.Read(p)
}

func (g *gateway) logGit(class policyClass, method, path, service, outcome string, status int) {
	if g.logger != nil {
		g.logger.Printf("gateway class=%s method=%s path=%s service=%s result=%s status=%d", class, method, path, service, outcome, status)
	}
}