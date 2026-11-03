// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

var pathParamRE = regexp.MustCompile(`\{[^}]*\}`)

// Every API route the server registers is described in docs/api/openapi.yaml (the document may describe more:
// planned routes). A route is found by its registration, h("METHOD "+apiPrefix+"/path", ...), in api*.go.
func TestEveryAPIRouteIsInTheSpec(t *testing.T) {
	b, err := os.ReadFile("../../docs/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(b)
	if err != nil {
		t.Fatal(err)
	}
	spec := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			spec[method+" "+pathParamRE.ReplaceAllString(path, "{}")] = true
		}
	}
	files, _ := filepath.Glob("api*.go")
	routeRE := regexp.MustCompile(`\bh\("([A-Z]+) "\+apiPrefix\+"([^"]*)"`)
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range routeRE.FindAllStringSubmatch(string(src), -1) {
			n++
			if key := m[1] + " " + pathParamRE.ReplaceAllString(m[2], "{}"); !spec[key] {
				t.Errorf("%s (%s) is registered but not in docs/api/openapi.yaml", strings.TrimSpace(m[1]+" "+m[2]), f)
			}
		}
	}
	if n < 30 {
		t.Errorf("found only %d registered routes: the pattern no longer matches how routes are registered", n)
	}
}
