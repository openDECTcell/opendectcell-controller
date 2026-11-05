// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

func statsEnv(t *testing.T) (*env, uuid.UUID, uuid.UUID) {
	e := newEnvWith(t, func(s *Server) { s.Stats = &stats.Store{Pool: s.Pool} })
	var uid, aid uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'admin'").Scan(&aid)
	return e, uid, aid
}

func TestStatisticsSeriesPage(t *testing.T) {
	e, uid, aid := statsEnv(t)
	var hall, line, ahs uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state, cap_audio_nb) VALUES ('Hall', 'active', 1) RETURNING id").Scan(&hall)
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Home', 'u', 'd.example') RETURNING id", uid).Scan(&line)
	e.store.Pool.Exec(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Admin line', 'a', 'd.example')", aid)
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x01', 1, '\\x', 'Kitchen') RETURNING id", uid).Scan(&ahs)
	// the user's line was up for 20 of the last 24 hours, then down
	e.store.Pool.Exec(ctx, "INSERT INTO sip_reg_event (account_id, at, state) VALUES ($1, now() - interval '5 days', 'registered'), ($1, now() - interval '4 hours', 'failed')", line)
	// two overlapping calls of the user at the hall (peak 2 against a capacity of 1), one with a poor quality, a call of the administrator
	call := func(owner uuid.UUID, startAgo, endAgo string, mos string, dir string) {
		q := `{}`
		if mos != "" {
			q = `{"mos": ` + mos + `}`
		}
		if _, err := e.store.Pool.Exec(ctx, `INSERT INTO call_log (owner_user_id, handset_id, sip_account_id, direction, started_at, answered_at, ended_at, cell_ids, quality)
			VALUES ($1, $2, $3, $6, now() - $4::interval, now() - $4::interval + interval '2 seconds', now() - $5::interval, ARRAY[$7::uuid], $8::jsonb)`,
			owner, ahs, line, startAgo, endAgo, dir, hall, q); err != nil {
			t.Fatal(err)
		}
	}
	call(uid, "50 minutes", "40 minutes", "4.2", "out")
	call(uid, "48 minutes", "30 minutes", "2.9", "in")
	e.store.Pool.Exec(ctx, `INSERT INTO call_log (owner_user_id, direction, started_at, answered_at, ended_at, quality) VALUES ($1, 'out', now() - interval '3 hours', now() - interval '3 hours', now() - interval '2 hours', '{"mos": 1.5}')`, aid)
	u, a := e.userBrowser(t), e.adminBrowser(t)

	resp, body := e.get(t, u, "/statistics?days=7")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user page: %d", resp.StatusCode)
	}
	for _, want := range []string{"against the 7 before", "Most calls at once, per day", "When the calls are", `class="chart heat"`, "Most calls at once, per base station", "full",
		"Call quality, by day", "VoIP lines", "Home", "Kitchen", "The worst calls", "MOS 2.9", "/statistics/export.csv?table=lines&amp;days=7", "unchanged"} {
		if !strings.Contains(body, want) && !strings.Contains(body, strings.ReplaceAll(want, "&amp;", "&")) {
			t.Errorf("the user's page lacks %q", want)
		}
	}
	// the line's availability: up for 5 days (counted from the window's start 7 days back: from 5 days ago), down 4 h: 116 h up, 4 h down = 96.7 %
	if !strings.Contains(body, "96.7 %") {
		t.Errorf("the line's registered share is not on the page")
	}
	for _, leak := range []string{"Admin line", "MOS 1.5", "/admin/statistics/usage"} {
		if strings.Contains(body, leak) {
			t.Errorf("the user's page shows %q", leak)
		}
	}
	// the administrator sees all of it, and the link to the report, but not a worst-call list (calls are their owners')
	_, ab := e.get(t, a, "/admin/statistics?days=7")
	for _, want := range []string{"Admin line", "Home", "/admin/statistics/usage", "/admin/statistics/export.csv?table=heat"} {
		if !strings.Contains(ab, want) && !strings.Contains(ab, strings.ReplaceAll(want, "&", "&amp;")) {
			t.Errorf("the administrator's page lacks %q", want)
		}
	}
	if strings.Contains(ab, "The worst calls") {
		t.Error("the administrators' page lists calls of other people")
	}
	// a line without a record says so; only a base station that reported a capacity can be "full" (Hall: 1, two calls at once)
	if i := strings.Index(ab, "Admin line"); i < 0 || !strings.Contains(ab[i:i+strings.Index(ab[i:], "</tr>")], "no record") {
		t.Error("a line without a registration record is shown with a share of time")
	}
	var shed uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('Shed', 'active') RETURNING id").Scan(&shed)
	e.store.Pool.Exec(ctx, "INSERT INTO call_log (owner_user_id, direction, started_at, answered_at, ended_at, cell_ids) VALUES ($1, 'out', now() - interval '10 minutes', now() - interval '9 minutes', now() - interval '5 minutes', ARRAY[$2::uuid])", uid, shed)
	if _, ab = e.get(t, a, "/admin/statistics?days=7"); strings.Count(ab, `>full</span>`) != 1 || !strings.Contains(ab, "not reported") {
		t.Errorf("full: %d times, want 1 (Hall only)", strings.Count(ab, `>full</span>`))
	}
}

// The administrators' table of VoIP lines holds the least available first, and not more than 30.
func TestAdministratorsSeeTheWorstLinesFirst(t *testing.T) {
	e, uid, _ := statsEnv(t)
	for i := range 33 {
		var id uuid.UUID
		e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, $2, 'u', 'd.example') RETURNING id", uid, fmt.Sprintf("line-%02d", i)).Scan(&id)
		switch i {
		case 7: // down for the last 3.5 of 7 days
			e.store.Pool.Exec(ctx, "INSERT INTO sip_reg_event (account_id, at, state) VALUES ($1, now() - interval '7 days', 'registered'), ($1, now() - interval '3 days 12 hours', 'failed')", id)
		case 8: // down for the last 16 hours
			e.store.Pool.Exec(ctx, "INSERT INTO sip_reg_event (account_id, at, state) VALUES ($1, now() - interval '7 days', 'registered'), ($1, now() - interval '16 hours', 'failed')", id)
		default: // up the whole week
			e.store.Pool.Exec(ctx, "INSERT INTO sip_reg_event (account_id, at, state) VALUES ($1, now() - interval '7 days', 'registered')", id)
		}
	}
	_, body := e.get(t, e.adminBrowser(t), "/admin/statistics?days=7")
	shown := 0
	for i := range 33 {
		if strings.Contains(body, fmt.Sprintf("line-%02d<", i)) {
			shown++
		}
	}
	if shown != 30 {
		t.Errorf("%d lines are on the page, want 30", shown)
	}
	half, tenth, good := strings.Index(body, "line-07<"), strings.Index(body, "line-08<"), strings.Index(body, "line-00<")
	if half < 0 || tenth < 0 || half > tenth || (good >= 0 && good < tenth) {
		t.Errorf("not in order of availability: half-down at %d, down the last 16 h at %d, a good line at %d", half, tenth, good)
	}
}

func TestHeatLevels(t *testing.T) {
	var h stats.Heat
	h.Cells[0][9], h.Cells[0][10], h.Cells[1][9] = 1, 10, 5
	h.Max, h.Total = 10, 16
	svg := string(heatmap(h))
	for _, want := range []string{`heat-1" x="`, `heat-4" x="`, "Mon 09:00 to 09:59: 1 call", "Mon 10:00 to 10:59: 10 calls", "Tue 09:00 to 09:59: 5 calls", "no calls"} {
		if !strings.Contains(svg, want) {
			t.Errorf("the heat map lacks %q", want)
		}
	}
	// five calls of ten is the middle: not the lightest, not the darkest
	idx := strings.Index(svg, "Tue 09:00 to 09:59: 5 calls")
	before := svg[:idx]
	cls := before[strings.LastIndex(before, `class="heat-cell `):]
	if !strings.HasPrefix(cls, `class="heat-cell heat-3"`) && !strings.HasPrefix(cls, `class="heat-cell heat-2"`) {
		t.Errorf("five of ten calls: %q", cls[:40])
	}
}

func TestStatisticsCSV(t *testing.T) {
	e, uid, aid := statsEnv(t)
	var line uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, '=cmd|calc', 'u', 'd.example') RETURNING id", uid).Scan(&line)
	e.store.Pool.Exec(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Secret admin line', 'a', 'd.example')", aid)
	e.store.Pool.Exec(ctx, `INSERT INTO call_log (owner_user_id, sip_account_id, direction, started_at, answered_at, ended_at, quality)
		VALUES ($1, $2, 'in', now() - interval '1 hour', now() - interval '59 minutes', now() - interval '50 minutes', '{"mos": 3.1}')`, uid, line)
	u, a := e.userBrowser(t), e.adminBrowser(t)
	for _, table := range csvTables {
		resp, body := e.get(t, u, "/statistics/export.csv?table="+table+"&days=7")
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") || !strings.Contains(resp.Header.Get("Content-Disposition"), "opendectcell-"+table+"-7d-") || !strings.HasPrefix(body, "\xef\xbb\xbf") {
			t.Errorf("%s: %d %q", table, resp.StatusCode, resp.Header)
			continue
		}
		if strings.Contains(body, "Secret admin line") {
			t.Errorf("%s: another user's line in the file", table)
		}
	}
	_, lines := e.get(t, u, "/statistics/export.csv?table=lines&days=7")
	if !strings.Contains(lines, "'=cmd|calc") || strings.Contains(lines, ",=cmd") || !strings.Contains(lines, "line,calls,answered,missed,talk_seconds,measured_calls,average_mos,poor_calls,registered_percent") ||
		!strings.Contains(lines, ",1,1,0,540,1,3.10,1,") {
		t.Errorf("lines CSV: %q", lines)
	}
	var evil uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('=HYPERLINK(1)', 'active') RETURNING id").Scan(&evil)
	e.store.Pool.Exec(ctx, "UPDATE call_log SET cell_ids = ARRAY[$1::uuid] WHERE owner_user_id = $2", evil, uid)
	for _, table := range []string{"stations", "station_peaks"} {
		if _, body := e.get(t, u, "/statistics/export.csv?table="+table+"&days=7"); !strings.Contains(body, "'=HYPERLINK(1)") || strings.Contains(body, "\n=HYPERLINK") {
			t.Errorf("%s CSV: %q", table, body)
		}
	}
	_, heat := e.get(t, u, "/statistics/export.csv?table=heat&days=7")
	if n := strings.Count(heat, "\n"); n != 169 {
		t.Errorf("heat CSV has %d lines, want 169", n)
	}
	if resp, _ := e.get(t, u, "/statistics/export.csv?table=nonsense"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown table: %d", resp.StatusCode)
	}
	if resp, _ := e.get(t, u, "/admin/statistics/export.csv?table=days"); resp.StatusCode == http.StatusOK {
		t.Error("a user got the administrators' export")
	}
	if resp, _ := e.get(t, e.browser(), "/statistics/export.csv?table=days"); resp.StatusCode == http.StatusOK {
		t.Error("an export without a login")
	}
	_, all := e.get(t, a, "/admin/statistics/export.csv?table=lines&days=7")
	if !strings.Contains(all, "Secret admin line") || !strings.Contains(all, "cmd|calc") {
		t.Errorf("the administrators' export lacks lines: %q", all)
	}
}

func TestUsageReport(t *testing.T) {
	e, uid, aid := statsEnv(t)
	now := time.Now()
	lastMonth := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, now.Location()).AddDate(0, -1, 0)
	e.store.Pool.Exec(ctx, "UPDATE app_user SET display_name = '=SUM(A1)' WHERE id = $1", uid)
	for i := 0; i < 3; i++ {
		e.store.Pool.Exec(ctx, `INSERT INTO call_log (owner_user_id, direction, started_at, answered_at, ended_at) VALUES ($1, 'out', $2, $2, $3)`, uid, lastMonth.Add(time.Duration(i)*time.Hour), lastMonth.Add(time.Duration(i)*time.Hour+10*time.Minute))
	}
	e.store.Pool.Exec(ctx, `INSERT INTO call_log (owner_user_id, direction, started_at, answered_at, ended_at) VALUES ($1, 'out', now() - interval '1 hour', now() - interval '1 hour', now() - interval '50 minutes')`, aid)
	var org uuid.UUID
	e.store.Pool.QueryRow(ctx, "INSERT INTO organization (name) VALUES ('Makerspace') RETURNING id").Scan(&org)
	e.store.Pool.Exec(ctx, "INSERT INTO organization_member (org_id, user_id, role) VALUES ($1, $2, 'member')", org, uid)
	a, u := e.adminBrowser(t), e.userBrowser(t)
	// the default is the month before this one
	resp, body := e.get(t, a, "/admin/statistics/usage")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, lastMonth.Format("January 2006")) || !strings.Contains(body, "Makerspace") || !strings.Contains(body, ">user<") {
		t.Fatalf("report: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "<td>3</td>") || !strings.Contains(body, "0.5 h") {
		t.Errorf("the user's 3 calls and 30 minutes are not in the report")
	}
	// this month holds the administrator's call, not the user's
	_, this := e.get(t, a, "/admin/statistics/usage?month="+now.Format("2006-01"))
	if !strings.Contains(this, now.Format("January 2006")) || strings.Contains(this, "0.5 h") {
		t.Errorf("this month's report")
	}
	// nonsense and the future become the default month
	if _, odd := e.get(t, a, "/admin/statistics/usage?month=2999-01"); !strings.Contains(odd, lastMonth.Format("January 2006")) {
		t.Error("a month in the future is accepted")
	}
	_, csvUsers := e.get(t, a, "/admin/statistics/usage.csv?table=users&month="+lastMonth.Format("2006-01"))
	if !strings.Contains(csvUsers, "username,name,calls,answered,talk_minutes,handsets,voip_lines") || !strings.Contains(csvUsers, "user,'=SUM(A1),3,3,30,0,0") {
		t.Errorf("users CSV: %q", csvUsers)
	}
	_, csvOrgs := e.get(t, a, "/admin/statistics/usage.csv?table=orgs&month="+lastMonth.Format("2006-01"))
	if !strings.Contains(csvOrgs, "Makerspace,1,3,3,30,0,0") {
		t.Errorf("orgs CSV: %q", csvOrgs)
	}
	for _, p := range []string{"/admin/statistics/usage", "/admin/statistics/usage.csv?table=users"} {
		if resp, body := e.get(t, u, p); resp.StatusCode == http.StatusOK || strings.Contains(body, "Makerspace") {
			t.Errorf("a user got %s", p)
		}
	}
}

func TestChangeText(t *testing.T) {
	for _, c := range []struct {
		cur, prev int
		want      string
	}{{5, 5, "unchanged"}, {6, 5, "+1 (+20 %)"}, {3, 4, "-1 (-25 %)"}, {4, 0, "+4 (new)"}, {0, 4, "-4 (-100 %)"}, {0, 0, "unchanged"}} {
		if got := change(c.cur, c.prev); got != c.want {
			t.Errorf("change(%d, %d) = %q, want %q", c.cur, c.prev, got, c.want)
		}
	}
	if got := change64(5400, 3600); got != "+0.5 h (+50 %)" {
		t.Errorf("change64: %q", got)
	}
}
