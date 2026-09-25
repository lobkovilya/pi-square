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
	for _, tc := range []struct {
		authority string
		host      string
		want      bool
	}{
		{"api.github.com", permittedHost, true},
		{"api.github.com:443", permittedHost, true},
		{"API.GitHub.com", permittedHost, true},
		{"github.com", gitHost, true},
		{"github.com:443", gitHost, true},
		{"github.com", permittedHost, false},
		{"api.github.com:80", permittedHost, false},
		{"api.github.com.evil.com", permittedHost, false},
		{"evil.com:443", gitHost, false},
		{"[::1]:443", gitHost, false},
	} {
		if got := hostMatches(tc.authority, tc.host); got != tc.want {
			t.Errorf("hostMatches(%q, %q) = %v, want %v", tc.authority, tc.host, got, tc.want)
		}
	}
}

func TestAuthorizeGit(t *testing.T) {
	for _, class := range []policyClass{classRO, classW} {
		if deny := authorizeGit(class, gitUploadPack); deny != nil {
			t.Errorf("%s upload-pack denied: %v", class, deny)
		}
	}
	if deny := authorizeGit(classRO, gitReceivePack); deny == nil || deny.code != denyWriteRequiresPublish {
		t.Errorf("RO receive-pack should require publish, got %v", deny)
	}
	if deny := authorizeGit(classW, gitReceivePack); deny != nil {
		t.Errorf("W receive-pack denied: %v", deny)
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