// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/media"
	"github.com/openDECTcell/opendectcell-controller/internal/media/rtpstats"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
)

type (
	sipStats = rtp.Stats
	cellRx   = rtpstats.Stats
)

// The admin call page shows each call's quality per leg, and says which leg
// is the worse one.
func TestAdminCalls(t *testing.T) {
	e := newEnv(t)
	admin := e.browser()
	e.login(t, admin, "admin", "admin password")
	if _, body := e.get(t, admin, "/admin/calls"); !strings.Contains(body, "No calls yet") {
		t.Error("empty call log")
	}

	s := &calllog.Store{Pool: e.store.Pool}
	start := time.Now().Add(-time.Hour)
	id, err := s.Start(ctx, calllog.Call{Direction: "out", RemoteParty: "210", StartedAt: start})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Answered(ctx, id, start.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	rtt, peer := 48.0, 2.5
	q := media.Summarise(rtpStats(), cellStats(), media.Stats{})
	q.SIP.RTTMs, q.SIP.PeerLossPct = &rtt, &peer
	q.WorseLeg = "cell"
	yes := true
	if err := s.Ended(ctx, id, calllog.End{At: start.Add(74 * time.Second), Reason: "normal", SIPCodec: "G722", CellCodec: "L16", SRTP: &yes, Quality: &q}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, calllog.Call{Direction: "in", RemoteParty: "+4930999", StartedAt: start.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}

	resp, body := e.get(t, admin, "/admin/calls")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if strings.Contains(body, "4930999") {
		t.Error("the admin call page shows a remote number in full")
	}
	for _, want := range []string{"1:12", "G722 · cell L16 · SRTP", "peer 2.5 %", "RTT 48.0 ms", "loss 10.0 %", "<strong>cell</strong>", "*****999", "not answered", "ongoing", "no media"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func rtpStats() (s sipStats) { return }

func cellStats() (s cellRx) {
	s.Received, s.Expected, s.Lost, s.LossRate = 900, 1000, 100, 0.1
	return
}

func fillCalls(t *testing.T, e *env) (annaHandset, annaLine, other string) {
	t.Helper()
	var uid, ben, hs, line, benLine uuid.UUID
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'user'").Scan(&uid)
	e.store.Pool.QueryRow(ctx, "SELECT id FROM app_user WHERE username = 'admin'").Scan(&ben)
	e.store.Pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x0a', 10, '\\x', 'Kitchen') RETURNING id", uid).Scan(&hs)
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Home', '1', 'sip.example.org') RETURNING id", uid).Scan(&line)
	e.store.Pool.QueryRow(ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Admins', '2', 'sip.example.org') RETURNING id", ben).Scan(&benLine)
	s := &calllog.Store{Pool: e.store.Pool}
	t0 := time.Now().Add(-3 * time.Hour)
	mk := func(owner uuid.UUID, dir, number string, answered bool, at time.Duration) int64 {
		id, err := s.Start(ctx, calllog.Call{OwnerID: &owner, HandsetID: &hs, SIPAccountID: &line, Direction: dir, RemoteParty: number, StartedAt: t0.Add(at)})
		if err != nil {
			t.Fatal(err)
		}
		if answered {
			s.Answered(ctx, id, t0.Add(at+2*time.Second))
		}
		yes := true
		q := media.Summarise(rtpStats(), cellStats(), media.Stats{})
		q.WorseLeg = "sip"
		if err := s.Ended(ctx, id, calllog.End{At: t0.Add(at + 75*time.Second), Reason: "normal", SIPCodec: "G722", CellCodec: "G722", SRTP: &yes, SIPTLS: &yes, Quality: &q}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mk(uid, "out", "+4930111", true, 0)
	mk(uid, "in", "+4930222", false, time.Hour)
	mk(uid, "in", "=HYPERLINK(1)", true, 2*time.Hour)
	mk(ben, "in", "+4930999", false, time.Hour)
	return hs.String(), line.String(), benLine.String()
}

// A user sees their own calls in full, with filters and a page for each call; nobody else's.
func TestUserCallsPage(t *testing.T) {
	e := newEnv(t)
	hs, line, other := fillCalls(t, e)
	u := e.userBrowser(t)
	_, body := e.get(t, u, "/calls")
	for _, want := range []string{"4930111", "4930222", "Kitchen", "Home", "answered", ">missed<", "new</span>", "MOS ", "1 missed call has not been seen yet", `href="/calls/export.csv?`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(body, "4930999") || strings.Contains(body, "Not built yet") {
		t.Errorf("another user's call, or the old stub: %s", body)
	}
	// filters
	for q, want := range map[string]struct{ has, hasNot string }{
		"dir=out":                     {"4930111", "4930222"},
		"dir=in":                      {"4930222", "4930111"},
		"dir=missed":                  {"4930222", "4930111"},
		"handset=" + hs:               {"4930111", ""},
		"line=" + line:                {"4930222", ""},
		"line=" + other:               {"4930222", ""}, // not the user's line: ignored, everything shows
		"handset=not-a-uuid&days=999": {"4930111", ""},
	} {
		_, b := e.get(t, u, "/calls?"+q)
		if !strings.Contains(b, want.has) || (want.hasNot != "" && strings.Contains(b, want.hasNot)) {
			t.Errorf("?%s: wants %q and not %q", q, want.has, want.hasNot)
		}
	}
	// the call's own page
	var id int64
	e.store.Pool.QueryRow(ctx, "SELECT id FROM call_log WHERE remote_party = '+4930111'").Scan(&id)
	_, one := e.get(t, u, "/calls/"+strconv.FormatInt(id, 10))
	for _, want := range []string{"4930111", "Kitchen", "Home", "G722", "How it was carried", "MOS ", "the connection to your provider or PBX", "ended normally"} {
		if !strings.Contains(one, want) {
			t.Errorf("the call page lacks %q", want)
		}
	}
	var benCall int64
	e.store.Pool.QueryRow(ctx, "SELECT id FROM call_log WHERE remote_party = '+4930999'").Scan(&benCall)
	if resp, _ := e.get(t, u, "/calls/"+strconv.FormatInt(benCall, 10)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another user's call: %d", resp.StatusCode)
	}
	if resp, _ := e.get(t, u, "/calls/zero"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a call that is no number: %d", resp.StatusCode)
	}
	// marking missed calls as read touches the user's own
	if r := e.post(t, u, "/calls/read", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("mark read: %d", r.StatusCode)
	}
	_, after := e.get(t, u, "/calls")
	if strings.Contains(after, "have not been seen") || strings.Contains(after, "has not been seen") || strings.Contains(after, "new</span>") {
		t.Errorf("still unread after marking")
	}
	var benUnread int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM call_log WHERE remote_party = '+4930999' AND missed_read_at IS NULL").Scan(&benUnread)
	if benUnread != 1 {
		t.Errorf("another user's missed call was marked: %d", benUnread)
	}
}

func TestCallsExport(t *testing.T) {
	e := newEnv(t)
	fillCalls(t, e)
	u := e.userBrowser(t)
	resp, body := e.get(t, u, "/calls/export.csv?dir=in")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export: %d %v", resp.StatusCode, resp.Header)
	}
	lines := strings.Split(strings.TrimSpace(strings.TrimPrefix(body, "\xEF\xBB\xBF")), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "started,direction,number,handset,line,answered,duration_seconds") {
		t.Fatalf("lines: %q", lines)
	}
	if strings.Contains(body, "4930111") || strings.Contains(body, "4930999") {
		t.Errorf("outgoing or foreign calls in an incoming export: %s", body)
	}
	// a number that a spreadsheet would run is made text; a telephone number is left alone
	if !strings.Contains(body, "'=HYPERLINK(1)") || !strings.Contains(body, ",+4930222,") {
		t.Errorf("csv guard: %s", body)
	}
	if resp, _ := e.get(t, e.browser(), "/calls/export.csv"); resp.StatusCode == http.StatusOK {
		t.Errorf("export without login")
	}
}

func TestCSVSafe(t *testing.T) {
	for in, want := range map[string]string{"": "", "030 123": "030 123", "+4930 12": "+4930 12", "+49x": "'+49x", "=1+1": "'=1+1", "-5": "'-5", "@SUM(1)": "'@SUM(1)", "\tx": "'\tx", "Kitchen": "Kitchen"} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
