// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"strings"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

func TestNiceStep(t *testing.T) {
	for most, want := range map[int]int{1: 1, 4: 1, 5: 2, 8: 2, 9: 5, 20: 5, 21: 10, 40: 10, 41: 20, 100: 50, 101: 50, 200: 50, 201: 100, 450: 200, 4000: 1000} {
		if got := niceStep(most); got != want {
			t.Errorf("niceStep(%d) = %d, want %d", most, got, want)
		}
	}
}

func TestCallsChartHasAnAxisAndHoverText(t *testing.T) {
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	days := []stats.Day{
		{Day: day, Calls: 0},
		{Day: day.Add(24 * time.Hour), Calls: 1, Answered: 1, TalkSeconds: 90},
		{Day: day.Add(48 * time.Hour), Calls: 7, Answered: 5, TalkSeconds: 1200},
	}
	svg := string(callsChart(days))
	// the axis: ticks 0, 2, 4, 6, 8 (the highest day has 7 calls) as labels, one line each
	for _, tick := range []string{">0<", ">2<", ">4<", ">6<", ">8<"} {
		if !strings.Contains(svg, `class="chart-label"`) || !strings.Contains(svg, tick) {
			t.Errorf("no y axis label %s: %s", tick, svg)
		}
	}
	if n := strings.Count(svg, `class="chart-grid"`); n != 5 {
		t.Errorf("%d grid lines, want 5", n)
	}
	// every day says what it is, also a day without calls; no <title> next to the script's tip
	for _, tip := range []string{`data-tip="Mon 2 Nov: no calls"`, `data-tip="Tue 3 Nov: 1 call, 1 answered, 1 min talk time"`, `data-tip="Wed 4 Nov: 7 calls, 5 answered, 20 min talk time"`} {
		if !strings.Contains(svg, tip) {
			t.Errorf("missing %s in %s", tip, svg)
		}
	}
	if strings.Contains(svg, "<title>") {
		t.Errorf("a native tooltip would double the script's: %s", svg)
	}
	if n := strings.Count(svg, `class="chart-hit"`); n != 3 {
		t.Errorf("%d hover areas, want one per day", n)
	}
	if !strings.Contains(svg, "<desc>") || !strings.Contains(svg, "Wed 4 Nov: 7 calls") {
		t.Errorf("no text alternative: %s", svg)
	}
}
