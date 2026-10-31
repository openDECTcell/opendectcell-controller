// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

// callsChart draws calls per day as inline SVG bars: the answered part solid,
// the rest lighter. No script, no style attribute (the page's CSP allows
// neither); colours come from the .bar classes in app.css, and each bar has
// a title and the chart a text alternative for screen readers.
func callsChart(days []stats.Day) template.HTML {
	const w, h, pad, label = 640, 160, 4, 18
	if len(days) == 0 {
		return ""
	}
	most := 1
	total := 0
	for _, d := range days {
		if d.Calls > most {
			most = d.Calls
		}
		total += d.Calls
	}
	slot := float64(w-2*pad) / float64(len(days))
	bw := slot * 0.7
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="Calls per day, %d in the last %d days">`, w, h, total, len(days))
	for i, d := range days {
		x := float64(pad) + slot*float64(i) + (slot-bw)/2
		top := float64(h - label)
		all := top * float64(d.Calls) / float64(most)
		ans := top * float64(d.Answered) / float64(most)
		fmt.Fprintf(&b, `<g><title>%s: %d calls, %d answered, %d min talk</title>`, d.Day.Format("Mon 2 Jan"), d.Calls, d.Answered, d.TalkSeconds/60)
		fmt.Fprintf(&b, `<rect class="bar bar-all" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2"/>`, x, top-all, bw, all)
		fmt.Fprintf(&b, `<rect class="bar bar-answered" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2"/></g>`, x, top-ans, bw, ans)
		if len(days) <= 16 || (len(days)-1-i)%7 == 0 {
			fmt.Fprintf(&b, `<text class="chart-label" x="%.1f" y="%d" text-anchor="middle">%s</text>`, x+bw/2, h-4, d.Day.Format("2.1."))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // G203: built from numbers and fixed-format dates only
}

// hms formats seconds as h:mm:ss, or m:ss under an hour.
func hms(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// hours formats seconds as hours with one decimal, "1.5 h".
func hours(sec int64) string { return fmt.Sprintf("%.1f h", float64(sec)/3600) }

// pct is 100*a/b, "–" when b is 0.
func pct(a, b int) string {
	if b == 0 {
		return "–"
	}
	return fmt.Sprintf("%d %%", (100*a+b/2)/b)
}
