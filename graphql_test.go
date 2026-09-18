//go:build linux

package main

import (
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
)

func TestClassifyGraphQL(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantKind ast.Operation
		wantDeny bool
	}{
		{"anonymous query", `{"query":"{ viewer { login } }"}`, ast.Query, false},
		{"explicit query", `{"query":"query { viewer { login } }"}`, ast.Query, false},
		{"mutation", `{"query":"mutation { __typename }"}`, ast.Mutation, false},
		{"subscription", `{"query":"subscription { x }"}`, ast.Subscription, false},
		{"query with alias and fragment", `{"query":"query { a: viewer { ...F } } fragment F on User { login }"}`, ast.Query, false},
		{"mutation disguised with alias", `{"query":"mutation { a: __typename }"}`, ast.Mutation, false},
		{"named selection query", `{"query":"query Q { viewer { login } } mutation M { __typename }","operationName":"Q"}`, ast.Query, false},
		{"named selection mutation", `{"query":"query Q { viewer { login } } mutation M { __typename }","operationName":"M"}`, ast.Mutation, false},
		{"ambiguous multi-op", `{"query":"query A { x } query B { y }"}`, "", true},
		{"unknown operation name", `{"query":"query A { x }","operationName":"Z"}`, "", true},
		{"malformed json", `{"query":`, "", true},
		{"batch array", `[{"query":"{ x }"}]`, "", true},
		{"duplicate query key", `{"query":"{ x }","query":"mutation { y }"}`, "", true},
		{"missing query", `{"operationName":"A"}`, "", true},
		{"non-string query", `{"query":123}`, "", true},
		{"persisted query", `{"extensions":{"persistedQuery":{"sha256Hash":"abc"}}}`, "", true},
		{"malformed graphql", `{"query":"query { "}`, "", true},
		{"trailing data", `{"query":"{ x }"} garbage`, "", true},
		{"no operation", `{"query":"fragment F on User { login }"}`, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op, deny := classifyGraphQL([]byte(tc.body))
			if tc.wantDeny {
				if deny == nil {
					t.Fatalf("expected denial, got operation %q", op.kind)
				}
				return
			}
			if deny != nil {
				t.Fatalf("unexpected denial: %v", deny)
			}
			if op.kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", op.kind, tc.wantKind)
			}
		})
	}
}
