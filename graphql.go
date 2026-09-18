//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

const (
	maxGraphQLBody   = 1 << 20 // 1 MiB
	maxGraphQLTokens = 15000
)

// graphQLOperation is the operation selected from a GraphQL request document.
type graphQLOperation struct {
	kind ast.Operation
	name string
}

// classifyGraphQL parses a GraphQL POST body and returns the operation that
// GitHub would execute. It is intentionally strict: anything it cannot
// unambiguously resolve to a single named or anonymous operation is rejected so
// that a mutation can never slip through the read-only policy disguised as a
// query.
func classifyGraphQL(body []byte) (graphQLOperation, *denial) {
	if len(body) > maxGraphQLBody {
		return graphQLOperation{}, newDenial("unsupported_request_format", 413, "GraphQL request body is too large")
	}

	fields, err := decodeStrictObject(body)
	if err != nil {
		return graphQLOperation{}, newDenial("unsupported_request_format", 400, "GraphQL body must be a single JSON object: "+err.Error())
	}

	if raw, ok := fields["extensions"]; ok {
		if usesPersistedQuery(raw) {
			return graphQLOperation{}, newDenial("unsupported_request_format", 400, "persisted GraphQL queries are not supported")
		}
	}

	queryRaw, ok := fields["query"]
	if !ok {
		return graphQLOperation{}, newDenial("unsupported_request_format", 400, "GraphQL body is missing a query string")
	}
	var query string
	if err := json.Unmarshal(queryRaw, &query); err != nil {
		return graphQLOperation{}, newDenial("unsupported_request_format", 400, "GraphQL query must be a string")
	}

	var operationName string
	if raw, ok := fields["operationName"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &operationName); err != nil {
			return graphQLOperation{}, newDenial("unsupported_request_format", 400, "GraphQL operationName must be a string")
		}
	}

	doc, parseErr := parser.ParseQueryWithTokenLimit(&ast.Source{Input: query}, maxGraphQLTokens)
	if parseErr != nil {
		return graphQLOperation{}, newDenial("unsupported_request_format", 400, "GraphQL document does not parse: "+parseErr.Error())
	}
	return selectOperation(doc, operationName)
}

// selectOperation applies GraphQL's operation-selection rules. Fragments and
// aliases are ignored here on purpose: only the operation type decides the
// policy, and neither can change it.
func selectOperation(doc *ast.QueryDocument, operationName string) (graphQLOperation, *denial) {
	if len(doc.Operations) == 0 {
		return graphQLOperation{}, newDenial("unsupported_request_format", 400, "GraphQL document defines no operation")
	}

	if operationName != "" {
		op := doc.Operations.ForName(operationName)
		if op == nil {
			return graphQLOperation{}, newDenial("unsupported_request_format", 400, fmt.Sprintf("GraphQL operation %q not found", operationName))
		}
		return graphQLOperation{kind: op.Operation, name: op.Name}, nil
	}

	if len(doc.Operations) > 1 {
		return graphQLOperation{}, newDenial("unsupported_request_format", 400, "GraphQL request selects no operation but the document defines several")
	}
	op := doc.Operations[0]
	return graphQLOperation{kind: op.Operation, name: op.Name}, nil
}

func usesPersistedQuery(rawExtensions json.RawMessage) bool {
	var ext map[string]json.RawMessage
	if err := json.Unmarshal(rawExtensions, &ext); err != nil {
		return false
	}
	_, ok := ext["persistedQuery"]
	return ok
}

// decodeStrictObject parses body as exactly one JSON object, rejecting arrays
// (request batches), trailing data, and duplicate keys. Duplicate keys are
// rejected because different JSON parsers disagree on which value wins, which is
// a classic way to smuggle one document past a validator and a different one to
// the upstream.
func decodeStrictObject(body []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("expected a JSON object")
	}

	fields := map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected a string key")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}

	// Consume the closing brace and confirm there is nothing after it.
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing data")
	}
	return fields, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
