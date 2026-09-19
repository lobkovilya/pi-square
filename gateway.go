//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
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

	// These hooks make destination validation and dialing independently
	// testable. Production instances use the system resolver and a direct dialer.
	lookupIP     func(context.Context, string) ([]netip.Addr, error)
	dialContext  func(context.Context, string, string) (net.Conn, error)
	connectionID atomic.Uint64
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
	dialer := &net.Dialer{Timeout: gatewayDial}
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
		lookupIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dialContext: dialer.DialContext,
	}, nil
}

func (g *gateway) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName != "" && normalizeHostname(hello.ServerName) != permittedHost {
				return nil, fmt.Errorf("unexpected SNI %q", hello.ServerName)
			}
			return &g.leaf, nil
		},
	}
}

// serveIngress handles one client connection arriving on a class ingress. It
// intercepts only the fixed GitHub API endpoint; all other public HTTPS
// destinations are byte-for-byte tunnels.
func (g *gateway) serveIngress(conn net.Conn, class policyClass) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(gatewayIdle))
	limited := &io.LimitedReader{R: conn, N: connectMaxHeader}
	br := bufio.NewReader(limited)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		g.writeConnectError(conn, http.StatusMethodNotAllowed, "only CONNECT is supported")
		return
	}

	host, port, err := normalizeConnectAuthority(req.Host)
	if err != nil {
		g.writeConnectError(conn, http.StatusBadRequest, "invalid CONNECT authority")
		return
	}
	if host == permittedHost && port != permittedPort {
		g.log(class, req.Method, req.Host, "-", denyUnsupportedHost, http.StatusForbidden)
		g.writeConnectError(conn, http.StatusForbidden, "unsupported CONNECT authority")
		return
	}
	if port != permittedPort {
		g.writeConnectError(conn, http.StatusForbidden, "unsupported CONNECT port")
		return
	}

	// ReadRequest may have consumed bytes sent optimistically after CONNECT.
	// Drain both its buffer and the remainder of the limited reader before
	// continuing with the underlying connection.
	buffered := io.MultiReader(br, conn)
	conn.SetReadDeadline(time.Time{})
	if host != permittedHost {
		g.servePassthrough(conn, buffered, class, host, port)
		return
	}

	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	tlsConn := tls.Server(&readerConn{Conn: conn, reader: buffered}, g.tlsConfig())
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	g.serveTunnel(tlsConn, class)
}

// readerConn preserves bytes a buffered CONNECT parser read ahead.
type readerConn struct {
	net.Conn
	reader io.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func normalizeConnectAuthority(authority string) (string, string, error) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || host == "" || port == "" {
		return "", "", fmt.Errorf("authority must contain host and port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("invalid port")
	}
	host = normalizeHostname(host)
	if host == "" || strings.ContainsAny(host, " /\\@") {
		return "", "", fmt.Errorf("invalid host")
	}
	return host, strconv.Itoa(n), nil
}

func normalizeHostname(host string) string {
	host = strings.ToLower(host)
	if strings.HasSuffix(host, ".") {
		host = strings.TrimSuffix(host, ".")
	}
	return host
}

var errNonPublicDestination = errors.New("destination is not public")

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
}

func isPublicDestination(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func (g *gateway) resolvePublic(ctx context.Context, host string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if !isPublicDestination(ip) {
			return netip.Addr{}, errNonPublicDestination
		}
		return ip, nil
	}
	lookup := g.lookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, name string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", name)
		}
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(ips) == 0 {
		return netip.Addr{}, fmt.Errorf("destination has no addresses")
	}
	// Reject the whole answer if any address is non-public. Selecting only a
	// public member of a mixed answer would make policy depend on DNS ordering.
	for _, ip := range ips {
		if !isPublicDestination(ip) {
			return netip.Addr{}, errNonPublicDestination
		}
	}
	return ips[0].Unmap(), nil
}

func (g *gateway) servePassthrough(client net.Conn, clientReader io.Reader, class policyClass, host, port string) {
	id := g.connectionID.Add(1)
	started := time.Now()
	connectedIP := "-"
	outcome := "resolve_error"
	var clientToUpstream, upstreamToClient int64
	defer func() {
		if g.logger != nil {
			g.logger.Printf("gateway tunnel_id=%d class=%s destination=%s:%s connected_ip=%s outcome=%s duration_ms=%d bytes_client_to_upstream=%d bytes_upstream_to_client=%d",
				id, class, host, port, connectedIP, outcome, time.Since(started).Milliseconds(), clientToUpstream, upstreamToClient)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), gatewayDial)
	defer cancel()
	ip, err := g.resolvePublic(ctx, host)
	if err != nil {
		if errors.Is(err, errNonPublicDestination) {
			outcome = "blocked_destination"
			g.writeConnectError(client, http.StatusForbidden, "destination is not public")
		} else {
			g.writeConnectError(client, http.StatusBadGateway, "destination resolution failed")
		}
		return
	}
	connectedIP = ip.String()
	dial := g.dialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: gatewayDial}).DialContext
	}
	upstream, err := dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		outcome = "dial_error"
		g.writeConnectError(client, http.StatusBadGateway, "upstream connection failed")
		return
	}
	defer upstream.Close()

	// Do not acknowledge CONNECT until DNS policy and the upstream connection
	// have both succeeded.
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		outcome = "ack_error"
		return
	}
	outcome = "complete"

	type copyResult struct {
		direction string
		n         int64
		err       error
	}
	results := make(chan copyResult, 2)
	go func() {
		n, err := io.Copy(upstream, clientReader)
		closeTunnelWrite(upstream)
		results <- copyResult{"upstream", n, err}
	}()
	go func() {
		n, err := io.Copy(client, upstream)
		closeTunnelWrite(client)
		results <- copyResult{"client", n, err}
	}()

	first := <-results
	// A half-close permits the peer's final TLS records to flow, while the
	// deadline guarantees a peer cannot strand the relay forever.
	client.SetDeadline(time.Now().Add(gatewayIdle))
	upstream.SetDeadline(time.Now().Add(gatewayIdle))
	second := <-results
	for _, result := range []copyResult{first, second} {
		if result.direction == "upstream" {
			clientToUpstream = result.n
		} else {
			upstreamToClient = result.n
		}
		if result.err != nil && !isClosedConnectionError(result.err) {
			outcome = "relay_error"
		}
	}
}

func closeTunnelWrite(conn net.Conn) {
	if conn, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = conn.CloseWrite()
	}
}

func isClosedConnectionError(err error) bool {
	return err == nil || strings.Contains(err.Error(), "use of closed network connection")
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
