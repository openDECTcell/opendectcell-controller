// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/backup"
	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
	"github.com/openDECTcell/opendectcell-controller/internal/webhook"
)

// family is one metric: its help, its type and its samples. The text format wants every sample of a metric together, after one HELP
// and one TYPE: the writer collects them here and writes them in name order.
type family struct {
	help, typ string
	samples   []string
}

type exposition struct {
	fams map[string]*family
}

func newExposition() *exposition { return &exposition{fams: map[string]*family{}} }

// label is one label pair with its value escaped.
func label(k, v string) string {
	return k + `="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
}

// add records a sample of the metric name (help and typ are those of its first sample).
func (e *exposition) add(name, typ, help string, v any, labels ...string) {
	e.addTo(name, name, typ, help, v, labels...)
}

// addTo records a sample called sample in the family fam: a histogram's _bucket, _sum and _count are samples of one family.
func (e *exposition) addTo(fam, sample, typ, help string, v any, labels ...string) {
	name := sample
	f := e.fams[fam]
	if f == nil {
		f = &family{help: help, typ: typ}
		e.fams[fam] = f
	}
	val := fmt.Sprint(v)
	if x, ok := v.(float64); ok {
		val = strconv.FormatFloat(x, 'g', -1, 64)
	}
	if len(labels) > 0 {
		f.samples = append(f.samples, name+"{"+strings.Join(labels, ",")+"} "+val)
		return
	}
	f.samples = append(f.samples, name+" "+val)
}

func (e *exposition) String() string {
	names := make([]string, 0, len(e.fams))
	for n := range e.fams {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		f := e.fams[n]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", n, f.help, n, f.typ)
		for _, s := range f.samples {
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// MOSBuckets are the upper bounds of the call quality histogram: 4 and above is good, below 3.5 poor (ITU-T G.107).
var MOSBuckets = []float64{2, 2.5, 3, 3.5, 4, 4.2, 4.4}

// callStats counts what the controller saw since it started: how calls ended and how good they sounded. Counters that restart at zero
// when the controller does are what Prometheus expects (it handles the reset).
type callStats struct {
	mu     sync.Mutex
	ended  map[string]uint64
	mos    []uint64 // the count of calls at or below each bucket's bound, cumulative at render time
	mosSum float64
	mosN   uint64
}

func (c *callStats) observe(result string, mos float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended == nil {
		c.ended, c.mos = map[string]uint64{}, make([]uint64, len(MOSBuckets))
	}
	c.ended[result]++
	if mos > 0 {
		for i, b := range MOSBuckets {
			if mos <= b {
				c.mos[i]++
				break
			}
		}
		c.mosSum += mos
		c.mosN++
	}
}

func (c *callStats) write(e *exposition) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range []string{"answered", "missed", "not answered", "other"} {
		e.add("opendectcell_calls_ended_total", "counter", "Calls that ended since the controller started, by how they went.", c.ended[r], label("result", r))
	}
	var cum uint64
	for i, b := range MOSBuckets {
		if c.mos != nil {
			cum += c.mos[i]
		}
		e.addTo("opendectcell_call_mos", "opendectcell_call_mos_bucket", "histogram", "The quality (MOS) of the calls that were measured, since the controller started.", cum, label("le", strconv.FormatFloat(b, 'g', -1, 64)))
	}
	e.addTo("opendectcell_call_mos", "opendectcell_call_mos_bucket", "histogram", "", c.mosN, label("le", "+Inf"))
	e.addTo("opendectcell_call_mos", "opendectcell_call_mos_sum", "histogram", "", strconv.FormatFloat(c.mosSum, 'g', -1, 64))
	e.addTo("opendectcell_call_mos", "opendectcell_call_mos_count", "histogram", "", c.mosN)
}

// observeCall is told when a call has ended (the call log's hook): it counts how it went and how it sounded.
func (s *Server) observeCall(ctx context.Context, id int64) {
	c, err := (&calllog.Store{Pool: s.Pool}).Get(ctx, id)
	if err != nil {
		return
	}
	result, _ := callResult(c)
	switch result {
	case "answered", "missed", "not answered":
	default:
		result = "other"
	}
	mos := 0.0
	if c.Quality != nil {
		mos = c.Quality.MOS
	}
	s.calls.observe(result, mos)
}

// metrics serves Prometheus text for a monitoring system. It exists only when a token is configured (OPENDECTCELL_HTTP_METRICS_TOKEN) and
// needs it as a bearer token; it names base stations, so it is not for the public. VoIP lines are by id only.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.MetricsToken == "" {
		http.NotFound(w, r)
		return
	}
	got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	want := sha256.Sum256([]byte(s.MetricsToken))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	e := newExposition()
	s.collectMetrics(ctx, e)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(e.String()))
}

func (s *Server) collectMetrics(ctx context.Context, e *exposition) {
	now := time.Now()
	e.add("opendectcell_build_info", "gauge", "Build information.", 1, label("version", s.Version))
	if !s.StartedAt.IsZero() {
		e.add("opendectcell_uptime_seconds", "gauge", "Seconds since the controller started.", int64(time.Since(s.StartedAt).Seconds()))
	}
	// the process
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	e.add("opendectcell_go_goroutines", "gauge", "Goroutines.", runtime.NumGoroutine())
	e.add("opendectcell_go_heap_bytes", "gauge", "Bytes of heap in use.", ms.HeapAlloc)
	// the database pool: acquired is what is in use, so near the maximum means requests wait
	if s.Pool != nil {
		st := s.Pool.Stat()
		e.add("opendectcell_db_pool_connections", "gauge", "Connections of the database pool, by state.", st.AcquiredConns(), label("state", "acquired"))
		e.add("opendectcell_db_pool_connections", "gauge", "", st.IdleConns(), label("state", "idle"))
		e.add("opendectcell_db_pool_max_connections", "gauge", "The most connections the pool opens.", st.MaxConns())
		e.add("opendectcell_db_pool_wait_seconds_total", "counter", "Time requests spent waiting for a connection.", st.EmptyAcquireWaitTime().Seconds())
	}
	if s.Stats != nil {
		if f, err := s.Stats.Fleet(ctx); err == nil {
			e.add("opendectcell_cells", "gauge", "Cells by state.", f.CellsActive, label("state", "active"))
			e.add("opendectcell_cells", "gauge", "", f.CellsPending, label("state", "pending"))
			e.add("opendectcell_cells", "gauge", "", f.CellsDisabled, label("state", "disabled"))
			e.add("opendectcell_users", "gauge", "Users that can log in.", f.Users)
			e.add("opendectcell_users_pending", "gauge", "Users waiting for approval.", f.UsersPending)
			e.add("opendectcell_cells_certificate_expiring", "gauge", "Active cells whose certificate ends within 14 days without a successor.", f.CertsExpiring)
		}
		if m, err := s.Stats.Summary(ctx, stats.Scope{}); err == nil {
			e.add("opendectcell_handsets", "gauge", "Handsets registered.", m.Handsets)
			e.add("opendectcell_calls_total", "counter", "Calls recorded, all time.", m.CallsTotal)
			e.add("opendectcell_calls_24h", "gauge", "Calls started in the last 24 hours.", m.Calls24h)
		}
	}
	// calls going on now, in all and on each base station
	active, err := (&calllog.Store{Pool: s.Pool}).Active(ctx, nil)
	if err == nil {
		e.add("opendectcell_calls_active", "gauge", "Calls going on now.", len(active))
	}
	onCell := map[string]int{}
	for _, c := range active {
		for _, id := range c.CellIDs {
			onCell[id.String()]++
		}
	}
	s.calls.write(e)
	// the base stations
	if s.Cells != nil {
		if cells, err := s.Cells.ListCells(ctx, &auth.User{Role: auth.RoleAdmin}); err == nil {
			sort.Slice(cells, func(i, j int) bool { return cells[i].Name < cells[j].Name })
			online := 0
			for _, c := range cells {
				if c.State != "active" {
					continue
				}
				up := 0
				if s.liveStatus(c.ID).Online {
					up, online = 1, online+1
				}
				l := label("cell", c.Name)
				e.add("opendectcell_cell_up", "gauge", "Whether an active cell's link is up.", up, l)
				e.add("opendectcell_cell_calls_active", "gauge", "Calls going on now on an active cell.", onCell[c.ID.String()], l)
				if c.CapAudioNB != nil {
					e.add("opendectcell_cell_capacity_calls", "gauge", "How many calls an active cell says it can carry at once.", int(*c.CapAudioNB), l)
				}
				if c.CertExpiresAt != nil {
					e.add("opendectcell_cell_certificate_expiry_seconds", "gauge", "Seconds until an active cell's newest certificate ends (negative: it has ended).", math.Round(c.CertExpiresAt.Sub(now).Seconds()), l)
				}
				if c.LastSeenAt != nil {
					e.add("opendectcell_cell_last_seen_timestamp_seconds", "gauge", "When an active cell was last heard, as a Unix time.", c.LastSeenAt.Unix(), l)
				}
			}
			e.add("opendectcell_cells_online", "gauge", "Active cells whose link is up.", online)
		}
	}
	// the VoIP lines: by state, and each by its id (a name could be somebody's)
	if s.SIP != nil {
		rows, err := s.Pool.Query(ctx, "SELECT id FROM sip_account WHERE enabled AND register ORDER BY id")
		if err == nil {
			byState := map[string]int{"registered": 0, "failed": 0, "backoff": 0, "registering": 0, "unknown": 0}
			for rows.Next() {
				var id uuid.UUID
				if rows.Scan(&id) != nil {
					continue
				}
				st, ok := s.SIP.Status(id)
				state := "unknown"
				if ok {
					state = string(st.State)
					if st.State == sip.RegDisabled {
						state = "unknown"
					}
				}
				byState[state]++
				up := 0
				if ok && st.State == sip.RegRegistered {
					up = 1
				}
				e.add("opendectcell_sip_line_registered", "gauge", "Whether a VoIP line that is switched on to register is registered.", up, label("line", id.String()))
			}
			rows.Close()
			for _, k := range []string{"registered", "failed", "backoff", "registering", "unknown"} {
				e.add("opendectcell_sip_lines", "gauge", "VoIP lines that are switched on to register, by registration state.", byState[k], label("state", k))
			}
		}
	}
	// the queues: webhooks and notification mail
	var pending, failed24 int
	var oldest *time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'pending'), count(*) FILTER (WHERE state = 'failed' AND done_at > now() - interval '24 hours'),
		min(created_at) FILTER (WHERE state = 'pending') FROM webhook_delivery`).Scan(&pending, &failed24, &oldest); err == nil {
		e.add("opendectcell_webhook_deliveries_pending", "gauge", "Webhook deliveries waiting to be posted or retried.", pending)
		e.add("opendectcell_webhook_deliveries_failed_24h", "gauge", "Webhook deliveries that were given up in the last 24 hours.", failed24)
		age := 0.0
		if oldest != nil {
			age = now.Sub(*oldest).Seconds()
		}
		e.add("opendectcell_webhook_oldest_pending_seconds", "gauge", "Age of the oldest webhook delivery that still waits.", math.Round(age))
		var off int
		if s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook WHERE NOT enabled AND disabled_reason <> ''").Scan(&off) == nil {
			e.add("opendectcell_webhooks_switched_off", "gauge", fmt.Sprintf("Webhooks switched off after %d failed deliveries in a row.", webhook.FailLimit), off)
		}
	}
	var mailWait int
	if s.Pool.QueryRow(ctx, "SELECT count(*) FROM notification WHERE mail_wanted AND mailed_at IS NULL").Scan(&mailWait) == nil {
		e.add("opendectcell_notification_mail_waiting", "gauge", "Notifications that wait for mail (quiet hours, or the mail server).", mailWait)
	}
	// the backups
	if s.BackupDir != "" {
		switch st, err := backup.Read(s.BackupDir); {
		case err == nil:
			ok := 0
			if st.Problem(now) == "" {
				ok = 1
			}
			e.add("opendectcell_backup_ok", "gauge", "1 if the last backup run held and the last good backup is not too old, else 0.", ok)
			if !st.LastGood.IsZero() {
				e.add("opendectcell_backup_last_success_timestamp_seconds", "gauge", "When the last good backup was made, as a Unix time.", st.LastGood.Unix())
			}
		case errors.Is(err, backup.ErrNone):
		default:
			e.add("opendectcell_backup_ok", "gauge", "1 if the last backup run held and the last good backup is not too old, else 0.", 0)
		}
		if free, total, err := backupSpace(s); err == nil && total > 0 {
			e.add("opendectcell_backup_volume_free_bytes", "gauge", "Free space on the backup volume.", free)
			e.add("opendectcell_backup_volume_total_bytes", "gauge", "Size of the backup volume.", total)
		}
	}
}

func backupSpace(s *Server) (uint64, uint64, error) {
	if s.freeSpace != nil {
		return s.freeSpace(s.BackupDir)
	}
	return backup.FreeSpace(s.BackupDir)
}
