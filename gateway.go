//go:build linux

package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxRequestBody   = 10 << 20 // 10 MiB
	gatewayIdle      = 90 * time.Second
	gatewayDial      = 15 * time.Second
	gatewayResponse  = 60 * time.Second
	connectMaxHeader = 1 << 16
)

// hopByHopHeaders are stripped in both directions; they describe a single
// transport hop and must not be relayed across the gateway.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// gateway is the single external process endpoint. One instance serves both
// ingress classes; the class is supplied by the caller (bound to which listener
// accepted the connection) and never inferred from client bytes.
type gateway struct {
	ca           *caMaterial
	leaf         tls.Certificate
	token        string
	client       *http.Client
	upstreamBase *url.URL
	logger       *log.Logger
}

func newGateway(ca *caMaterial, token string, logger *log.Logger) (*gateway, error) {
	leaf, err := ca.leafFor(permittedHost)
	if err != nil {
		return nil, err
	}
	base := &url.URL{Scheme: "https", Host: permittedHost + ":" + permittedPort}
	transport := &http.Transport{
		Proxy: nil, // never honor inherited outbound proxy settings
		DialContext: (&net.Dialer{
			Timeout: gatewayDial,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       gatewayIdle,
		TLSHandshakeTimeout:   gatewayDial,
		ResponseHeaderTimeout: gatewayResponse,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: permittedHost,
		},
	}
	return &gateway{
		ca:    ca,
		leaf:  leaf,
		token: token,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		upstreamBase: base,
		logger:       logger,
	}, nil
}

func (g *gateway) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName != "" && !strings.EqualFold(hello.ServerName, permittedHost) {
				return nil, fmt.Errorf("unexpected SNI %q", hello.ServerName)
			}
			return &g.leaf, nil
		},
	}
}

// serveIngress handles one client connection arriving on a class ingress: the
// HTTP CONNECT preamble, TLS interception, then the plaintext HTTP requests.
func (g *gateway) serveIngress(conn net.Conn, class policyClass) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(gatewayIdle))
	br := bufio.NewReader(io.LimitReader(conn, connectMaxHeader))
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		g.writeConnectError(conn, http.StatusMethodNotAllowed, "only CONNECT is supported")
		return
	}
	if !hostMatches(req.Host) {
		g.log(class, req.Method, req.Host, "-", denyUnsupportedHost, http.StatusForbidden)
		g.writeConnectError(conn, http.StatusForbidden, "unsupported CONNECT authority")
		return
	}
	if br.Buffered() > 0 {
		// Bytes after the CONNECT headers would desync TLS; refuse.
		g.writeConnectError(conn, http.StatusBadRequest, "unexpected data after CONNECT")
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	conn.SetReadDeadline(time.Time{})
	tlsConn := tls.Server(conn, g.tlsConfig())
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	g.serveTunnel(tlsConn, class)
}

func (g *gateway) serveTunnel(tlsConn *tls.Conn, class policyClass) {
	br := bufio.NewReader(tlsConn)
	for {
		tlsConn.SetReadDeadline(time.Now().Add(gatewayIdle))
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		resp, keepAlive := g.dispatch(class, req)
		sanitizeResponseHeaders(resp.Header)
		resp.Close = !keepAlive
		tlsConn.SetWriteDeadline(time.Now().Add(gatewayResponse))
		if err := resp.Write(tlsConn); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// dispatch validates and authorizes a single tunneled request and returns the
// response to send back plus whether the connection may be reused.
func (g *gateway) dispatch(class policyClass, req *http.Request) (*http.Response, bool) {
	keepAlive := !req.Close

	if !hostMatches(req.Host) {
		return g.denialResponse(req, newDenial(denyUnsupportedHost, http.StatusForbidden, "unsupported request authority")), false
	}
	if req.Header.Get("Upgrade") != "" {
		return g.denialResponse(req, newDenial(denyUnsupportedRequest, http.StatusBadRequest, "protocol upgrades are not supported")), false
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody+1))
	req.Body.Close()
	if err != nil {
		return g.denialResponse(req, newDenial(denyUnsupportedRequest, http.StatusBadRequest, "could not read request body")), false
	}
	if len(body) > maxRequestBody {
		return g.denialResponse(req, newDenial(denyUnsupportedRequest, http.StatusRequestEntityTooLarge, "request body is too large")), false
	}

	if deny := authorizeRequest(class, req.Method, req.URL.Path, req.Header, body); deny != nil {
		g.log(class, req.Method, req.URL.Path, "-", deny.code, deny.status)
		return g.denialResponse(req, deny), keepAlive
	}

	resp, err := g.forward(class, req, body)
	if err != nil {
		g.log(class, req.Method, req.URL.Path, "-", "upstream_error", http.StatusBadGateway)
		return g.denialResponse(req, newDenial("upstream_error", http.StatusBadGateway, "upstream request failed")), false
	}
	g.log(class, req.Method, req.URL.Path, "allow", "", resp.StatusCode)
	return resp, keepAlive
}

// forward builds a fresh upstream request from validated fields rather than
// replaying decrypted bytes, replaces the credential, and returns the upstream
// response with its body buffered so framing is unambiguous.
func (g *gateway) forward(class policyClass, req *http.Request, body []byte) (*http.Response, error) {
	target := *g.upstreamBase
	target.Path = req.URL.Path
	target.RawQuery = req.URL.RawQuery

	upstream, err := http.NewRequest(req.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	upstream.Header = cloneAllowedHeaders(req.Header)
	upstream.Header.Set("Authorization", "Bearer "+g.token)
	upstream.Host = permittedHost
	if len(body) > 0 {
		upstream.ContentLength = int64(len(body))
	}

	resp, err := g.client.Do(upstream)
	if err != nil {
		return nil, err
	}
	buffered, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBody+1))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(buffered))
	resp.ContentLength = int64(len(buffered))
	resp.TransferEncoding = nil
	resp.Uncompressed = false
	return resp, nil
}

func (g *gateway) denialResponse(req *http.Request, d *denial) *http.Response {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]string{"code": d.code, "message": d.message},
	})
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("X-Pi-Square-Denial", d.code)
	return &http.Response{
		StatusCode:    d.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(payload)),
		ContentLength: int64(len(payload)),
		Request:       req,
	}
}

func (g *gateway) writeConnectError(conn net.Conn, status int, message string) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(message), message)
}

func (g *gateway) log(class policyClass, method, path, decision, code string, status int) {
	if g.logger == nil {
		return
	}
	detail := decision
	if code != "" {
		detail = "deny:" + code
	}
	g.logger.Printf("gateway class=%s method=%s path=%s result=%s status=%d", class, method, path, detail, status)
}

// cloneAllowedHeaders copies request headers minus hop-by-hop, credential, and
// framing headers so the credential the gateway installs is the only one that
// reaches upstream.
func cloneAllowedHeaders(src http.Header) http.Header {
	dst := http.Header{}
	drop := connectionTokens(src)
	drop["Authorization"] = true
	drop["Cookie"] = true
	drop["Host"] = true
	drop["Content-Length"] = true
	for _, h := range hopByHopHeaders {
		drop[http.CanonicalHeaderKey(h)] = true
	}
	for key, values := range src {
		if drop[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
	return dst
}

func sanitizeResponseHeaders(header http.Header) {
	for key := range connectionTokens(header) {
		header.Del(key)
	}
	for _, h := range hopByHopHeaders {
		header.Del(h)
	}
	header.Del("Set-Cookie")
}

// connectionTokens returns the set of header names named in a Connection header,
// which are themselves hop-by-hop.
func connectionTokens(header http.Header) map[string]bool {
	tokens := map[string]bool{}
	for _, value := range header["Connection"] {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				tokens[http.CanonicalHeaderKey(token)] = true
			}
		}
	}
	return tokens
}
