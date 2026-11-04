// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

// callsChart draws calls per day as inline SVG bars: the answered part solid,
// the rest lighter, over a y axis with whole-number ticks. No style attribute
// (the page's CSP allows none); colours come from the .bar and .chart classes
// in app.css. Each day is a group carrying its text in data-tip, which app.js
// shows next to the pointer, and the chart has a text alternative for screen
// readers (a desc with every day).
func callsChart(days []stats.Day) template.HTML {
	const w, h, pad, label, left = 640, 180, 4, 18, 36
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
	step := niceStep(most)
	ticks := (most + step - 1) / step
	axisMax := ticks * step
	top := float64(h - label)
	head := 8.0 // room above the highest line for its label
	plot := top - head
	slot := float64(w-left-pad) / float64(len(days))
	bw := slot * 0.7
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="Calls per day, %d in the last %d days"><desc>`, w, h, total, len(days))
	for _, d := range days {
		fmt.Fprintf(&b, "%s: %s. ", d.Day.Format("Mon 2 Jan"), dayText(d))
	}
	b.WriteString(`</desc>`)
	for t := 0; t <= ticks; t++ {
		y := top - plot*float64(t*step)/float64(axisMax)
		fmt.Fprintf(&b, `<line class="chart-grid" x1="%d" y1="%.1f" x2="%d" y2="%.1f"/>`, left, y, w-pad, y)
		fmt.Fprintf(&b, `<text class="chart-label" x="%d" y="%.1f" text-anchor="end">%d</text>`, left-6, y+3.5, t*step)
	}
	for i, d := range days {
		x := float64(left) + slot*float64(i) + (slot-bw)/2
		all := plot * float64(d.Calls) / float64(axisMax)
		ans := plot * float64(d.Answered) / float64(axisMax)
		fmt.Fprintf(&b, `<g class="day" data-tip="%s: %s">`, d.Day.Format("Mon 2 Jan"), dayText(d))
		fmt.Fprintf(&b, `<rect class="chart-hit" x="%.1f" y="0" width="%.1f" height="%.1f"/>`, float64(left)+slot*float64(i), slot, top)
		fmt.Fprintf(&b, `<rect class="bar bar-all" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2"/>`, x, top-all, bw, all)
		fmt.Fprintf(&b, `<rect class="bar bar-answered" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2"/></g>`, x, top-ans, bw, ans)
		if len(days) <= 16 || (len(days)-1-i)%7 == 0 {
			fmt.Fprintf(&b, `<text class="chart-label" x="%.1f" y="%d" text-anchor="middle">%s</text>`, x+bw/2, h-4, d.Day.Format("2.1."))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // G203: built from numbers and fixed-format dates only
}

// dayText is what a day's bar says when it is hovered: no call is said so.
func dayText(d stats.Day) string {
	if d.Calls == 0 {
		return "no calls"
	}
	return fmt.Sprintf("%d %s, %d answered, %d min talk time", d.Calls, plural(d.Calls, "call", "calls"), d.Answered, d.TalkSeconds/60)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// niceStep is the distance between the y axis' ticks: 1, 2, 5, 10, 20, 50 ...
// the smallest that puts the highest value within four steps.
func niceStep(most int) int {
	for mag := 1; ; mag *= 10 {
		for _, m := range []int{1, 2, 5} {
			if s := m * mag; (most+s-1)/s <= 4 {
				return s
			}
		}
	}
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
