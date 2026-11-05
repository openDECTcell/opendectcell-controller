// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/media"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
)

var (
	metricNameRE   = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	sampleLineRE   = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})? (\S+)$`)
	labelPairRE    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\.)*"$`)
	histogramParts = []string{"_bucket", "_sum", "_count"}
)

// validExposition checks what the Prometheus text format asks of a page, and returns the samples by "name{labels}" (the value as text).
func validExposition(t *testing.T, text string) map[string]string {
	t.Helper()
	typeOf := map[string]string{}
	var order []string
	helped := map[string]bool{}
	sample := map[string]string{}
	var current string // the family whose samples are being written
	closed := map[string]bool{}
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "# HELP "):
			name := strings.Fields(l)[2]
			if helped[name] {
				t.Errorf("HELP of %s twice", name)
			}
			helped[name] = true
			order = append(order, name)
		case strings.HasPrefix(l, "# TYPE "):
			f := strings.Fields(l)
			if _, dup := typeOf[f[2]]; dup {
				t.Errorf("TYPE of %s twice", f[2])
			}
			typeOf[f[2]] = f[3]
			if current != "" {
				closed[current] = true
			}
			current = f[2]
		case l == "":
			t.Error("an empty line")
		default:
			m := sampleLineRE.FindStringSubmatch(l)
			if m == nil {
				t.Errorf("not a sample: %q", l)
				continue
			}
			name := m[1]
			family := name
			for _, p := range histogramParts {
				if typeOf[strings.TrimSuffix(name, p)] == "histogram" && strings.HasSuffix(name, p) {
					family = strings.TrimSuffix(name, p)
				}
			}
			if family != current {
				t.Errorf("sample %q is not in its family's block (%q)", l, current)
			}
			if closed[family] {
				t.Errorf("the samples of %s are not together", family)
			}
			if !metricNameRE.MatchString(name) || typeOf[family] == "" || !helped[family] {
				t.Errorf("%q has no HELP and TYPE before it", l)
			}
			if m[3] != "" {
				for _, p := range splitLabels(m[3]) {
					if !labelPairRE.MatchString(p) {
						t.Errorf("a bad label %q in %q", p, l)
					}
				}
			}
			if _, err := strconv.ParseFloat(m[4], 64); err != nil && m[4] != "+Inf" {
				t.Errorf("a bad value in %q", l)
			}
			key := name + m[2]
			if _, dup := sample[key]; dup {
				t.Errorf("the sample %s twice", key)
			}
			sample[key] = m[4]
		}
	}
	if !sort.StringsAreSorted(order) {
		t.Errorf("the metrics are not in name order: %v", order)
	}
	return sample
}

// splitLabels splits a label list at the commas that are not inside a quoted value.
func splitLabels(s string) []string {
	var out []string
	var cur strings.Builder
	in, esc := false, false
	for _, r := range s {
		switch {
		case esc:
			esc = false
		case r == '\\' && in:
			esc = true
		case r == '"':
			in = !in
		case r == ',' && !in:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	return append(out, cur.String())
}

func TestMetricsExposition(t *testing.T) {
	h := newHealthEnv(t, true)
	app := h.app
	app.MetricsToken = strings.Repeat("m", 32)
	app.Version = `1.2"3`
	app.StartedAt = time.Now().Add(-90 * time.Second)
	uid := uidOf(h.env, "user")
	// two active base stations (one up, with a capacity and a certificate), one of them with an awkward name
	var hall, odd uuid.UUID
	h.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state, cap_audio_nb, last_seen_at) VALUES ('Hall', 'active', 3, now()) RETURNING id").Scan(&hall)
	h.store.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ($1, 'active') RETURNING id", "A \"quoted\"\\name\nwith a break").Scan(&odd)
	h.store.Pool.Exec(ctx, "INSERT INTO cell_certificate (fingerprint, cell_id, serial, not_before, not_after) VALUES (decode(repeat('ab', 32), 'hex'), $1, '1', now() - interval '1 day', now() + interval '10 days')", hall)
	h.link.set(hall, true)
	// a call going on at the hall, lines (one up, one failing, one off), webhooks waiting, mail waiting, a backup, the volume
	h.store.Pool.Exec(ctx, "INSERT INTO call_log (owner_user_id, direction, started_at, cell_ids) VALUES ($1, 'in', now() - interval '2 minutes', ARRAY[$2::uuid])", uid, hall)
	var l1, l2 uuid.UUID
	h.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Somebody''s private line', 'a', 'd.example') RETURNING id", uid).Scan(&l1)
	h.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'l2', 'b', 'd.example') RETURNING id", uid).Scan(&l2)
	h.store.Pool.Exec(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain, enabled) VALUES ($1, 'off', 'c', 'd.example', false)", uid)
	h.statuses[l1] = sip.RegStatus{State: sip.RegRegistered}
	h.statuses[l2] = sip.RegStatus{State: sip.RegFailed}
	var l3 uuid.UUID
	h.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'l3', 'd', 'd.example') RETURNING id", uid).Scan(&l3)
	h.statuses[l3] = sip.RegStatus{State: sip.RegDisabled} // switched on to register but the line says it is off: not counted as failing
	wid := uuid.New()
	h.store.Pool.Exec(ctx, "INSERT INTO webhook (id, user_id, name, url, events, secret_enc) VALUES ($1, $2, 'w', 'https://example.org/x', ARRAY['call.ended'], '\\x00')", wid, uid)
	h.store.Pool.Exec(ctx, "INSERT INTO webhook_delivery (webhook_id, event, payload, created_at) VALUES ($1, 'call.ended', '{}', now() - interval '10 minutes'), ($1, 'call.ended', '{}', now())", wid)
	h.store.Pool.Exec(ctx, "INSERT INTO webhook_delivery (webhook_id, event, payload, state, done_at) VALUES ($1, 'call.ended', '{}', 'failed', now()), ($1, 'call.ended', '{}', 'failed', now() - interval '3 days')", wid)
	h.store.Pool.Exec(ctx, "INSERT INTO notification (user_id, kind, level, subject, mail_wanted) VALUES ($1, 'line', 'danger', 's', true)", uid)
	now := time.Now().UTC().Truncate(time.Second)
	if err := os.WriteFile(filepath.Join(h.dir, "status.json"), []byte(statusJSON(true, now.Add(-time.Hour), "")), 0o600); err != nil {
		t.Fatal(err)
	}
	h.app.freeSpace = func(string) (uint64, uint64, error) { return 40e9, 100e9, nil }
	// calls that ended: how they went and how they sounded
	for _, c := range []struct {
		answered bool
		dir      string
		mos      float64
	}{{true, "out", 4.3}, {true, "in", 3.2}, {false, "in", 0}, {true, "out", 1.8}, {false, "out", 0}, {true, "out", 3.5}} {
		var ans any
		if c.answered {
			ans = time.Now().Add(-time.Minute)
		}
		var id int64
		h.store.Pool.QueryRow(ctx, "INSERT INTO call_log (owner_user_id, direction, started_at, answered_at, ended_at) VALUES ($1, $2, now() - interval '5 minutes', $3, now()) RETURNING id", uid, c.dir, ans).Scan(&id)
		if c.mos > 0 {
			q, _ := json.Marshal(&media.Quality{MOS: c.mos})
			h.store.Pool.Exec(ctx, "UPDATE call_log SET quality = $2::jsonb WHERE id = $1", id, q)
		}
		app.CallEvent(ctx, calllog.EventEnded, id)
	}
	waitForCond(t, "the calls to be counted", func() bool {
		app.calls.mu.Lock()
		defer app.calls.mu.Unlock()
		var n uint64
		for _, v := range app.calls.ended {
			n += v
		}
		return n == 6
	})

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+app.MetricsToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(resp)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("/metrics: %d %v", resp.StatusCode, resp.Header)
	}
	if f := os.Getenv("OPENDECTCELL_DUMP_METRICS"); f != "" { // to look at the page, or to give it to promtool check metrics
		_ = os.WriteFile(f, []byte(body), 0o644) //nolint:gosec // G306: a test dump, to be read by another user
	}
	got := validExposition(t, body)
	check := func(key, want string) {
		t.Helper()
		if v, ok := got[key]; !ok || v != want {
			t.Errorf("%s = %q (present %v), want %q", key, v, ok, want)
		}
	}
	check(`opendectcell_cells{state="active"}`, "2")
	check(`opendectcell_cells_online`, "1")
	check(`opendectcell_cell_up{cell="Hall"}`, "1")
	check(`opendectcell_cell_up{cell="A \"quoted\"\\name\nwith a break"}`, "0")
	check(`opendectcell_cell_calls_active{cell="Hall"}`, "1")
	check(`opendectcell_cell_capacity_calls{cell="Hall"}`, "3")
	check(`opendectcell_calls_active`, "1")
	check(`opendectcell_sip_lines{state="registered"}`, "1")
	check(`opendectcell_sip_lines{state="failed"}`, "1")
	check(`opendectcell_sip_lines{state="unknown"}`, "1")
	check(`opendectcell_sip_line_registered{line="`+l1.String()+`"}`, "1")
	check(`opendectcell_sip_line_registered{line="`+l2.String()+`"}`, "0")
	check(`opendectcell_webhook_deliveries_pending`, "2")
	check(`opendectcell_webhook_deliveries_failed_24h`, "1")
	check(`opendectcell_notification_mail_waiting`, "1")
	check(`opendectcell_backup_ok`, "1")
	check(`opendectcell_backup_volume_free_bytes`, "40000000000")
	check(`opendectcell_calls_ended_total{result="answered"}`, "4")
	check(`opendectcell_calls_ended_total{result="missed"}`, "1")
	check(`opendectcell_calls_ended_total{result="not answered"}`, "1")
	// the histogram: 4.3, 3.2 and 1.8 were measured; buckets are cumulative; +Inf is the count
	check(`opendectcell_call_mos_bucket{le="2"}`, "1")
	check(`opendectcell_call_mos_bucket{le="3.5"}`, "3") // 3.5 itself is "at most 3.5"
	check(`opendectcell_call_mos_bucket{le="4.2"}`, "3")
	check(`opendectcell_call_mos_bucket{le="4.4"}`, "4")
	check(`opendectcell_call_mos_bucket{le="+Inf"}`, "4")
	check(`opendectcell_call_mos_count`, "4")
	check(`opendectcell_call_mos_sum`, "12.8")
	if v := got[`opendectcell_cell_certificate_expiry_seconds{cell="Hall"}`]; v == "" || mustFloat(t, v) < 9*86400 || mustFloat(t, v) > 10*86400 {
		t.Errorf("the certificate expiry of the hall: %q", v)
	}
	if v := got[`opendectcell_uptime_seconds`]; mustFloat(t, v) < 90 {
		t.Errorf("uptime %s", v)
	}
	if v := got[`opendectcell_webhook_oldest_pending_seconds`]; mustFloat(t, v) < 590 || mustFloat(t, v) > 700 {
		t.Errorf("the oldest pending delivery: %s s", v)
	}
	for _, name := range []string{"opendectcell_db_pool_max_connections", "opendectcell_go_goroutines", "opendectcell_go_heap_bytes", "opendectcell_backup_last_success_timestamp_seconds"} {
		if _, ok := got[name]; !ok {
			t.Errorf("no %s", name)
		}
	}
	// a backup that failed turns the gauge to 0, and the next scrape says so
	if err := os.WriteFile(filepath.Join(h.dir, "status.json"), []byte(statusJSON(false, now, "no space left")), 0o600); err != nil {
		t.Fatal(err)
	}
	req2, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/metrics", nil)
	req2.Header.Set("Authorization", "Bearer "+app.MetricsToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	if got2 := validExposition(t, readBody(resp2)); got2["opendectcell_backup_ok"] != "0" {
		t.Errorf("a failed backup: opendectcell_backup_ok = %q", got2["opendectcell_backup_ok"])
	}
	// no VoIP line is named, nobody's handset or number is in it
	for _, leak := range []string{"Somebody", "private line", "example.org", "d.example", "whsec_"} {
		if strings.Contains(body, leak) {
			t.Errorf("the metrics hold %q", leak)
		}
	}
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("%q is not a number", s)
	}
	return f
}
