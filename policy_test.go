//go:build linux

package main

import "testing"

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/graphql":         "/graphql",
		"/graphql?query=x": "/graphql",
		"/graphql/":        "/graphql",
		"/graphql/../x":    "/x",
		"/repos//owner":    "/repos/owner",
		"":                 "/",
		"/a/../../etc":     "/etc",
		"graphql":          "/graphql",
	}
	for input, want := range cases {
		if got := normalizePath(input); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsGraphQLPath(t *testing.T) {
	yes := []string{"/graphql", "/graphql?query=%7Bx%7D", "/graphql/"}
	no := []string{"/graphql/foo", "/rate_limit", "/repos/graphql", "/"}
	for _, p := range yes {
		if !isGraphQLPath(p) {
			t.Errorf("isGraphQLPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if isGraphQLPath(p) {
			t.Errorf("isGraphQLPath(%q) = true, want false", p)
		}
	}
}

func TestHostMatches(t *testing.T) {
	yes := []string{"api.github.com", "api.github.com:443", "API.GitHub.com"}
	no := []string{"github.com", "api.github.com:80", "api.github.com.evil.com", "evil.com:443", "[::1]:443"}
	for _, h := range yes {
		if !hostMatches(h) {
			t.Errorf("hostMatches(%q) = false, want true", h)
		}
	}
	for _, h := range no {
		if hostMatches(h) {
			t.Errorf("hostMatches(%q) = true, want false", h)
		}
	}
}

func TestIsJSONContentType(t *testing.T) {
	yes := []string{"application/json", "application/json; charset=utf-8", "application/json;charset=UTF-8"}
	no := []string{"", "text/plain", "application/graphql", "multipart/form-data"}
	for _, v := range yes {
		if !isJSONContentType(v) {
			t.Errorf("isJSONContentType(%q) = false, want true", v)
		}
	}
	for _, v := range no {
		if isJSONContentType(v) {
			t.Errorf("isJSONContentType(%q) = true, want false", v)
		}
	}
}

func TestAuthorizeRESTMethods(t *testing.T) {
	for _, method := range []string{"GET", "HEAD"} {
		if deny := authorizeREST(classRO, method); deny != nil {
			t.Errorf("RO %s denied: %v", method, deny)
		}
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if deny := authorizeREST(classRO, method); deny == nil || deny.code != denyWriteRequiresPublish {
			t.Errorf("RO %s should require publish, got %v", method, deny)
		}
		if deny := authorizeREST(classW, method); deny != nil {
			t.Errorf("W %s denied: %v", method, deny)
		}
	}
	if deny := authorizeREST(classW, "TRACE"); deny == nil || deny.code != denyUnsupportedMethod {
		t.Errorf("TRACE should be unsupported, got %v", deny)
	}
}
