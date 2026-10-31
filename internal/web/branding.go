// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"strconv"
)

// logo serves the instance's logo. It is public: the login page shows it.
// The type was sniffed when it was stored; nosniff and a sandboxing CSP keep
// a browser from treating it as anything else.
func (s *Server) logo(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		http.NotFound(w, r)
		return
	}
	ct, data, up, ok, err := s.Settings.Logo(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("Last-Modified", up.UTC().Format(http.TimeFormat))
	w.Write(data)
}
