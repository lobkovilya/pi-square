//go:build linux

package main

import (
	"mime"
	"net"
	"net/http"
	"path"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// The only upstream this gateway will ever talk to.
const (
	permittedHost = "api.github.com"
	permittedPort = "443"
)

// denial is a refused request. The code is a stable identifier the extension
// can branch on; write_requires_publish is the only one that offers a mode
// escalation. The HTTP status and message are what the client sees.
type denial struct {
	code    string
	status  int
	message string
}

func newDenial(code string, status int, message string) *denial {
	return &denial{code: code, status: status, message: message}
}

func (d *denial) Error() string {
	return d.code + ": " + d.message
}

// Recognizable denial codes. These are part of the contract with the extension
// and the acceptance tests, so keep them stable.
const (
	denyWriteRequiresPublish = "write_requires_publish"
	denyUnsupportedHost      = "unsupported_host"
	denyUnsupportedRequest   = "unsupported_request_format"
	denyUnsupportedMethod    = "unsupported_method"
)

// policyClass is fixed for the lifetime of an ingress. It is never derived from
// anything the client sends.
type policyClass string

const (
	classRO policyClass = "ro"
	classW  policyClass = "w"
)

func (c policyClass) writable() bool { return c == classW }

var restReadMethods = map[string]bool{
	http.MethodGet:  true,
	http.MethodHead: true,
}

var restWriteMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// authorizeRequest decides whether a fully parsed request is permitted under
// the given class. It classifies the endpoint first so that the GraphQL rules
// always run before the REST method rule and a mutation can never fall through
// to the REST read allowance.
func authorizeRequest(class policyClass, method, requestPath string, header http.Header, body []byte) *denial {
	if isGraphQLPath(requestPath) {
		return authorizeGraphQL(class, method, header, body)
	}
	return authorizeREST(class, method)
}

func authorizeREST(class policyClass, method string) *denial {
	if restReadMethods[method] {
		return nil
	}
	if restWriteMethods[method] {
		if class.writable() {
			return nil
		}
		return newDenial(denyWriteRequiresPublish, http.StatusForbidden,
			method+" is a write and requires publish mode")
	}
	return newDenial(denyUnsupportedMethod, http.StatusMethodNotAllowed,
		"method "+method+" is not supported")
}

func authorizeGraphQL(class policyClass, method string, header http.Header, body []byte) *denial {
	if method != http.MethodPost {
		return newDenial(denyUnsupportedRequest, http.StatusMethodNotAllowed,
			"the GraphQL endpoint only accepts POST")
	}
	if !isJSONContentType(header.Get("Content-Type")) {
		return newDenial(denyUnsupportedRequest, http.StatusUnsupportedMediaType,
			"the GraphQL endpoint requires Content-Type application/json")
	}
	if enc := header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return newDenial(denyUnsupportedRequest, http.StatusUnsupportedMediaType,
			"GraphQL request body encoding "+enc+" is not supported")
	}

	op, deny := classifyGraphQL(body)
	if deny != nil {
		return deny
	}
	switch op.kind {
	case ast.Query:
		return nil
	case ast.Mutation:
		if class.writable() {
			return nil
		}
		return newDenial(denyWriteRequiresPublish, http.StatusForbidden,
			"GraphQL mutations require publish mode")
	default:
		return newDenial(denyUnsupportedRequest, http.StatusForbidden,
			"GraphQL "+string(op.kind)+" operations are not supported")
	}
}

// isGraphQLPath matches only the exact normalized GraphQL endpoint. Every other
// spelling (a trailing slash, a subpath, a query string carrying the document)
// is deliberately not treated as GraphQL so it cannot inherit the REST read
// allowance; such requests are handled by the REST rules and, for the variants
// that matter, rejected there.
func isGraphQLPath(requestPath string) bool {
	return normalizePath(requestPath) == "/graphql"
}

func normalizePath(requestPath string) string {
	if i := strings.IndexAny(requestPath, "?#"); i >= 0 {
		requestPath = requestPath[:i]
	}
	if requestPath == "" {
		return "/"
	}
	cleaned := path.Clean(requestPath)
	if cleaned == "." {
		return "/"
	}
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

func isJSONContentType(value string) bool {
	if value == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

func hostMatches(authority string) bool {
	host := authority
	if h, p, err := net.SplitHostPort(authority); err == nil {
		if p != permittedPort {
			return false
		}
		host = h
	} else if strings.Contains(authority, ":") {
		return false
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host == permittedHost
}
