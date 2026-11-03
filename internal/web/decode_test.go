// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeTakesExactlyOneValue(t *testing.T) {
	type body struct {
		A int `json:"a"`
	}
	for _, c := range []struct {
		in string
		ok bool
	}{
		{`{"a":1}`, true},
		{"{\"a\":1}\n  \n", true},
		{`{"a":1}{"a":2}`, false},
		{`{"a":1} x`, false},
		{`{"a":1}[]`, false},
		{`{"b":1}`, false},
		{`{"a":`, false},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(c.in))
		var v body
		if got := decode(w, r, &v); got != c.ok {
			t.Errorf("%q: decode = %v, want %v (status %d)", c.in, got, c.ok, w.Code)
		}
	}
}
