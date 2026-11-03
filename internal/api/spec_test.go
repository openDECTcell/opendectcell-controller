// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

const specPath = "../../docs/api/openapi.yaml"

var rules = map[string]bool{"any": true, "user": true, "admin": true, "owner-or-admin": true, "may-add-cells": true, "owner-or-admin-shared-read": true,
	"org-member": true, "org-admin": true, "org-owner": true}

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	l := openapi3.NewLoader()
	doc, err := l.LoadFromData(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("invalid OpenAPI document: %v", err)
	}
	return doc
}

// Every operation names its authorisation rule (first-tasks.md A4: "each
// endpoint names its authorisation rule").
func TestEveryOperationHasAnAuthzRule(t *testing.T) {
	doc := loadSpec(t)
	n := 0
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			n++
			raw, ok := op.Extensions["x-authz"]
			rule, _ := raw.(string)
			if !ok || !rules[rule] {
				t.Errorf("%s %s: x-authz %v is missing or not one of the documented rules", method, path, raw)
			}
			if op.OperationID == "" {
				t.Errorf("%s %s: no operationId", method, path)
			}
			if rule == "any" && (op.Security == nil || len(*op.Security) != 0) {
				t.Errorf("%s %s: x-authz any but security is not empty", method, path)
			}
			// Callers who may not see an object, or an admin-only route,
			// get 404, so the spec must say so.
			if (rule == "admin" || (strings.Contains(path, "{") && rule != "user" && rule != "any")) && op.Responses.Status(404) == nil {
				t.Errorf("%s %s: restricted operation without a 404 response", method, path)
			}
		}
	}
	if n < 40 {
		t.Errorf("only %d operations; the spec looks truncated", n)
	}
}

// Secrets are write-only in every schema that has them.
func TestSecretsAreWriteOnly(t *testing.T) {
	doc := loadSpec(t)
	var walk func(name string, s *openapi3.Schema, seen map[*openapi3.Schema]bool)
	walk = func(name string, s *openapi3.Schema, seen map[*openapi3.Schema]bool) {
		if s == nil || seen[s] {
			return
		}
		seen[s] = true
		for p, ref := range s.Properties {
			if (p == "password" || p == "current" || p == "new" || strings.HasSuffix(p, "_secret")) && !ref.Value.WriteOnly {
				t.Errorf("%s.%s is a secret but not writeOnly", name, p)
			}
			walk(name+"."+p, ref.Value, seen)
		}
		for _, sub := range s.AllOf {
			walk(name, sub.Value, seen)
		}
		if s.Items != nil {
			walk(name+"[]", s.Items.Value, seen)
		}
	}
	for name, ref := range doc.Components.Schemas {
		walk(name, ref.Value, map[*openapi3.Schema]bool{})
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.RequestBody == nil {
				continue
			}
			for _, mt := range op.RequestBody.Value.Content {
				walk(method+" "+path, mt.Schema.Value, map[*openapi3.Schema]bool{})
			}
		}
	}
}
