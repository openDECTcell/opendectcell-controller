// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"html/template"
	"strings"

	"rsc.io/qr"
)

// qrSVG draws text as a QR code, an inline SVG that the page's content policy needs no exception for. The code is
// black on white whatever the theme is, with the four-module quiet zone scanners need.
func qrSVG(text string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4
	n := code.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="QR code"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n, n, n)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return template.HTML(b.String()), nil //nolint:gosec // G203: built here from integers only
}
