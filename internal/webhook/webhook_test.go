// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/netguard"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
	"github.com/openDECTcell/opendectcell-controller/internal/webhook"
)

var ctx = context.Background()

type fixture struct {
	s         *webhook.Store
	d         *webhook.Dispatcher
	anna, ben uuid.UUID
	now       time.Time
	srv       *httptest.Server
	mu        sync.Mutex
	got       []received
	code      int // what the receiver answers
}

type received struct {
	header http.Header
	body   []byte
}

// newFixture has a receiver on loopback and a store that may reach it (private networks, loopback and plain http allowed: tests only).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.Migrated(t)
	ks, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(ks)
	kr, err := db.LoadKeyring(ctx, pool, mk, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), code: 200}
	pol := netguard.Policy{AllowPrivate: true, AllowLoopback: true}
	f.s = &webhook.Store{Pool: pool, Keyring: kr, Policy: pol, Now: func() time.Time { return f.now }}
	f.d = &webhook.Dispatcher{Store: f.s, Client: webhook.NewClient(pol)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, received{r.Header.Clone(), b})
		code := f.code
		f.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(f.srv.Close)
	for name, id := range map[string]*uuid.UUID{"anna": &f.anna, "ben": &f.ben} {
		if err := pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source) VALUES ($1, 'user', 'local') RETURNING id", name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) setCode(c int) { f.mu.Lock(); f.code = c; f.mu.Unlock() }
func (f *fixture) received() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.got...)
}

func (f *fixture) hook(t *testing.T, user uuid.UUID, events ...string) (webhook.Hook, string) {
	t.Helper()
	h, sec, err := f.s.Create(ctx, user, webhook.Input{Name: "HA", URL: f.srv.URL + "/hook/secret-path-token", Events: events, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return h, sec
}

func TestURLRules(t *testing.T) {
	strict := &webhook.Store{} // the default policy: https only, nothing on the inside
	lax := &webhook.Store{Policy: netguard.Policy{AllowPrivate: true}}
	for _, c := range []struct {
		url    string
		store  *webhook.Store
		wantOK bool
	}{
		{"https://example.org/hook", strict, true},
		{"https://example.org:8443/a/b?x=1", strict, true},
		{"https://203.0.113.7/hook", strict, true},
		{"https://[2001:db8::1]/hook", strict, true},
		{"http://example.org/hook", strict, false},
		{"http://example.org/hook", lax, true},
		{"http://10.0.0.5:8123/api/webhook/x", strict, false},
		{"http://10.0.0.5:8123/api/webhook/x", lax, true},
		{"https://10.0.0.5/hook", strict, false},
		{"https://10.0.0.5/hook", lax, true},
		{"https://127.0.0.1/hook", lax, false},
		{"https://[::1]/hook", lax, false},
		{"https://169.254.169.254/latest/meta-data", lax, false},
		{"https://0.0.0.0/", lax, false},
		{"ftp://example.org/hook", lax, false},
		{"javascript:alert(1)", lax, false},
		{"https://user:pass@example.org/hook", strict, false},
		{"https://example.org/hook#frag", strict, false},
		{"https:///hook", strict, false},
		{"https://example.org/a b", strict, false},
		{"", strict, false},
		{"example.org/hook", strict, false},
		{"https://example.org/" + strings.Repeat("a", 500), strict, false},
	} {
		err := c.store.CheckURL(c.url)
		var v webhook.ValidationError
		if c.wantOK != (err == nil) || (err != nil && !errors.As(err, &v)) {
			t.Errorf("%q (private %v): %v, want ok=%v", c.url, c.store.Policy.AllowPrivate, err, c.wantOK)
		}
	}
}

func TestCreateListUpdateDelete(t *testing.T) {
	f := newFixture(t)
	h, sec, err := f.s.Create(ctx, f.anna, webhook.Input{Name: " Home Assistant ", URL: f.srv.URL + "/x", Events: []string{"call.missed", "call.incoming", "call.missed"}, Enabled: true, Auth: "Bearer abc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sec, "whsec_") || len(sec) != 6+43 || h.Name != "Home Assistant" || strings.Join(h.Events, ",") != "call.incoming,call.missed" || !h.HasAuth || h.Format != "json" {
		t.Fatalf("hook %+v secret %q", h, sec)
	}
	// the secret and the Authorization value are sealed: not in the table in the clear
	var n int
	f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook WHERE secret_enc = $1 OR position($1 in secret_enc) > 0 OR position($2 in auth_enc) > 0", []byte(sec), []byte("Bearer abc")).Scan(&n)
	if n != 0 {
		t.Error("a secret is stored in the clear")
	}
	f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE details::text LIKE '%'||$1||'%' OR details::text LIKE '%Bearer%' OR details::text LIKE '%secret-path%'", sec).Scan(&n)
	if n != 0 {
		t.Error("a secret is in the audit log")
	}
	// ben sees and touches none of it
	if l, _ := f.s.List(ctx, f.ben); len(l) != 0 {
		t.Errorf("ben sees %d webhooks", len(l))
	}
	if _, err := f.s.Get(ctx, f.ben, h.ID); !errors.Is(err, webhook.ErrNotFound) {
		t.Errorf("ben gets anna's: %v", err)
	}
	if _, err := f.s.Update(ctx, f.ben, h.ID, webhook.Input{Name: "x", URL: f.srv.URL, Events: []string{"call.ended"}, Enabled: true}); !errors.Is(err, webhook.ErrNotFound) {
		t.Errorf("ben updates anna's: %v", err)
	}
	if _, err := f.s.RotateSecret(ctx, f.ben, h.ID); !errors.Is(err, webhook.ErrNotFound) {
		t.Errorf("ben rotates anna's: %v", err)
	}
	if err := f.s.Delete(ctx, f.ben, h.ID); !errors.Is(err, webhook.ErrNotFound) {
		t.Errorf("ben deletes anna's: %v", err)
	}
	// an update keeps the Authorization value unless told, and can clear it
	h2, err := f.s.Update(ctx, f.anna, h.ID, webhook.Input{Name: "HA 2", URL: f.srv.URL + "/y", Format: "ntfy", Events: []string{"notification"}, Enabled: true})
	if err != nil || h2.Name != "HA 2" || h2.Format != "ntfy" || !h2.HasAuth || h2.URL != f.srv.URL+"/y" {
		t.Fatalf("update: %+v %v", h2, err)
	}
	if h2, err = f.s.Update(ctx, f.anna, h.ID, webhook.Input{Name: "HA 2", URL: f.srv.URL + "/y", Events: []string{"notification"}, Enabled: true, ClearAuth: true}); err != nil || h2.HasAuth {
		t.Errorf("clear: %+v %v", h2, err)
	}
	// switching it on again forgets the failures
	f.s.Pool.Exec(ctx, "UPDATE webhook SET enabled = false, failures = 10, disabled_reason = 'off' WHERE id = $1", h.ID)
	if h2, _ = f.s.Update(ctx, f.anna, h.ID, webhook.Input{Name: "HA 2", URL: f.srv.URL + "/y", Events: []string{"notification"}, Enabled: true}); h2.Failures != 0 || h2.DisabledReason != "" || !h2.Enabled {
		t.Errorf("re-enabled: %+v", h2)
	}
	// validation
	for name, in := range map[string]webhook.Input{
		"no name":    {URL: f.srv.URL, Events: []string{"call.ended"}},
		"long name":  {Name: strings.Repeat("x", 65), URL: f.srv.URL, Events: []string{"call.ended"}},
		"no events":  {Name: "x", URL: f.srv.URL},
		"bad event":  {Name: "x", URL: f.srv.URL, Events: []string{"call.exploded"}},
		"ping":       {Name: "x", URL: f.srv.URL, Events: []string{"ping"}},
		"bad format": {Name: "x", URL: f.srv.URL, Events: []string{"call.ended"}, Format: "xml"},
		"bad url":    {Name: "x", URL: "ftp://x", Events: []string{"call.ended"}},
		"auth lines": {Name: "x", URL: f.srv.URL, Events: []string{"call.ended"}, Auth: "a\nb"},
	} {
		var v webhook.ValidationError
		if _, _, err := f.s.Create(ctx, f.anna, in); !errors.As(err, &v) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// at most ten
	for i := 1; i < webhook.MaxPerUser; i++ {
		if _, _, err := f.s.Create(ctx, f.anna, webhook.Input{Name: "w", URL: f.srv.URL, Events: []string{"call.ended"}}); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, _, err := f.s.Create(ctx, f.anna, webhook.Input{Name: "w", URL: f.srv.URL, Events: []string{"call.ended"}}); err == nil {
		t.Error("an eleventh webhook")
	}
	if err := f.s.Delete(ctx, f.anna, h.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.Create(ctx, f.anna, webhook.Input{Name: "w", URL: f.srv.URL, Events: []string{"call.ended"}}); err != nil {
		t.Errorf("after a delete: %v", err)
	}
}

func TestDeliveryIsSignedAndHasTheEnvelope(t *testing.T) {
	f := newFixture(t)
	h, sec := f.hook(t, f.anna, "call.missed", "call.ended")
	if _, _, err := f.s.Create(ctx, f.anna, webhook.Input{Name: "off", URL: f.srv.URL + "/off", Events: []string{"call.missed"}, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	other, _, _ := f.s.Create(ctx, f.ben, webhook.Input{Name: "ben's", URL: f.srv.URL + "/ben", Events: []string{"call.missed"}, Enabled: true})
	_ = other
	// who gets it: the enabled webhooks of that user that subscribed to the event
	if n, err := f.s.Emit(ctx, f.anna, "call.missed", map[string]any{"remote_party": "+4930123", "summary": "Missed call from +4930123"}); err != nil || n != 1 {
		t.Fatalf("emit: %d %v", n, err)
	}
	if n, _ := f.s.Emit(ctx, f.anna, "call.incoming", map[string]any{}); n != 0 {
		t.Errorf("an event nobody subscribed to was queued %d times", n)
	}
	if n, err := f.d.Once(ctx); err != nil || n != 1 {
		t.Fatalf("once: %d %v", n, err)
	}
	got := f.received()
	if len(got) != 1 {
		t.Fatalf("%d posts", len(got))
	}
	r := got[0]
	var env struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		CreatedAt time.Time       `json:"created_at"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(r.body, &env); err != nil || env.Type != "call.missed" || env.ID == "" || !env.CreatedAt.Equal(f.now) || !strings.Contains(string(env.Data), "+4930123") {
		t.Fatalf("body %s (%v)", r.body, err)
	}
	if r.header.Get("Content-Type") != "application/json" || r.header.Get("X-OpenDECTcell-Event") != "call.missed" || r.header.Get("X-OpenDECTcell-Delivery") != env.ID || r.header.Get("User-Agent") != "opendectcell-controller" {
		t.Errorf("headers: %v", r.header)
	}
	// the signature: t=<unix>,v1=hex(HMAC-SHA256(secret, "<t>.<body>"))
	sig := r.header.Get("X-OpenDECTcell-Signature")
	parts := strings.Split(sig, ",")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "v1=") {
		t.Fatalf("signature %q", sig)
	}
	m := hmac.New(sha256.New, []byte(sec))
	m.Write([]byte(strings.TrimPrefix(parts[0], "t=") + "." + string(r.body)))
	if strings.TrimPrefix(parts[1], "v1=") != hex.EncodeToString(m.Sum(nil)) {
		t.Errorf("the signature does not verify with the secret that was shown")
	}
	if webhook.Sign([]byte(sec), f.now, r.body) != sig {
		t.Error("Sign is not what was sent")
	}
	if strings.Contains(string(r.body), "secret-path-token") || strings.Contains(sig, sec) {
		t.Error("the address or the secret is in what was sent")
	}
	// the log
	ds, _ := f.s.Deliveries(ctx, f.anna, h.ID, 10)
	if len(ds) != 1 || ds[0].State != "done" || ds[0].Attempts != 1 || ds[0].Code == nil || *ds[0].Code != 200 {
		t.Errorf("deliveries: %+v", ds)
	}
	if other, _ := f.s.Deliveries(ctx, f.ben, h.ID, 10); len(other) != 0 {
		t.Error("ben reads anna's log")
	}
	// the secret that stopped being the secret: a rotated one signs the next, the old one does not verify
	newSec, err := f.s.RotateSecret(ctx, f.anna, h.ID)
	if err != nil || newSec == sec {
		t.Fatalf("rotate: %v", err)
	}
	f.s.Emit(ctx, f.anna, "call.ended", map[string]any{})
	f.d.Once(ctx)
	r2 := f.received()[1]
	if webhook.Sign([]byte(newSec), f.now, r2.body) != r2.header.Get("X-OpenDECTcell-Signature") || webhook.Sign([]byte(sec), f.now, r2.body) == r2.header.Get("X-OpenDECTcell-Signature") {
		t.Error("after a rotation the new secret signs, and the old one does not")
	}
}

func TestAuthorizationHeaderAndTheNtfyFormat(t *testing.T) {
	f := newFixture(t)
	_, _, err := f.s.Create(ctx, f.anna, webhook.Input{Name: "ntfy", URL: f.srv.URL + "/topic", Format: "ntfy", Events: []string{"call.missed", "notification"}, Enabled: true, Auth: "Bearer tk_secret"})
	if err != nil {
		t.Fatal(err)
	}
	f.s.Emit(ctx, f.anna, "call.missed", map[string]any{"summary": "Missed call from Anna", "remote_party": "+49"})
	f.s.Emit(ctx, f.anna, "notification", map[string]any{"subject": "The VoIP line Home cannot register", "level": "danger"})
	f.d.Once(ctx)
	got := f.received()
	if len(got) != 2 {
		t.Fatalf("%d posts", len(got))
	}
	byTitle := map[string]received{}
	for _, r := range got {
		byTitle[r.header.Get("Title")] = r
		if r.header.Get("Authorization") != "Bearer tk_secret" || !strings.HasPrefix(r.header.Get("Content-Type"), "text/plain") || r.header.Get("X-OpenDECTcell-Signature") == "" {
			t.Errorf("headers: %v", r.header)
		}
	}
	m, n := byTitle["Missed call"], byTitle["opendectcell"]
	if string(m.body) != "Missed call from Anna" || m.header.Get("Priority") != "high" || m.header.Get("Tags") != "call.missed" {
		t.Errorf("missed: %q %v", m.body, m.header)
	}
	if string(n.body) != "The VoIP line Home cannot register" || n.header.Get("Priority") != "high" {
		t.Errorf("notification: %q %v", n.body, n.header)
	}
}

func TestRetriesBackoffAndGivingUp(t *testing.T) {
	f := newFixture(t)
	h, _ := f.hook(t, f.anna, "call.ended")
	f.setCode(503)
	f.s.Emit(ctx, f.anna, "call.ended", map[string]any{})
	delays := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}
	for i, d := range delays {
		if n, _ := f.d.Once(ctx); n != 1 {
			t.Fatalf("attempt %d: %d deliveries due", i+1, n)
		}
		ds, _ := f.s.Deliveries(ctx, f.anna, h.ID, 1)
		if ds[0].State != "pending" || ds[0].Attempts != i+1 || !ds[0].NextAt.Equal(f.now.Add(d)) || !strings.Contains(ds[0].Error, "503") {
			t.Fatalf("after attempt %d: %+v (want next in %v)", i+1, ds[0], d)
		}
		// not due yet: nothing happens
		if n, _ := f.d.Once(ctx); n != 0 {
			t.Fatalf("a delivery was posted before its time (%d)", n)
		}
		f.now = f.now.Add(d)
	}
	if n, _ := f.d.Once(ctx); n != 1 {
		t.Fatal("the sixth attempt")
	}
	ds, _ := f.s.Deliveries(ctx, f.anna, h.ID, 1)
	if ds[0].State != "failed" || ds[0].Attempts != 6 || !strings.Contains(ds[0].Error, "given up") {
		t.Errorf("after the last attempt: %+v", ds[0])
	}
	if len(f.received()) != 6 {
		t.Errorf("%d posts, want 6", len(f.received()))
	}
	if got, _ := f.s.Get(ctx, f.anna, h.ID); got.Failures != 1 || !got.Enabled || !strings.HasPrefix(got.LastStatus, "failed") {
		t.Errorf("hook after one failed delivery: %+v", got)
	}
	// a receiver that recovers: the next one is delivered and the failures are forgotten
	f.setCode(200)
	f.s.Emit(ctx, f.anna, "call.ended", map[string]any{})
	f.d.Once(ctx)
	if got, _ := f.s.Get(ctx, f.anna, h.ID); got.Failures != 0 || got.LastStatus != "delivered" {
		t.Errorf("after a delivery: %+v", got)
	}
}

func TestWhatIsNotRetried(t *testing.T) {
	f := newFixture(t)
	h, _ := f.hook(t, f.anna, "call.ended")
	for _, code := range []int{400, 401, 404, 410, 301, 302} {
		f.setCode(code)
		f.s.Emit(ctx, f.anna, "call.ended", map[string]any{})
		f.d.Once(ctx)
		ds, _ := f.s.Deliveries(ctx, f.anna, h.ID, 1)
		if ds[0].State != "failed" || ds[0].Attempts != 1 || ds[0].Code == nil || *ds[0].Code != code {
			t.Errorf("%d: %+v", code, ds[0])
		}
	}
	for _, code := range []int{408, 429, 500, 502} {
		f.setCode(code)
		f.s.Emit(ctx, f.anna, "call.ended", map[string]any{})
		f.d.Once(ctx)
		ds, _ := f.s.Deliveries(ctx, f.anna, h.ID, 1)
		if ds[0].State != "pending" {
			t.Errorf("%d: %+v", code, ds[0])
		}
	}
}

func TestAHookThatKeepsFailingIsSwitchedOff(t *testing.T) {
	f := newFixture(t)
	h, _ := f.hook(t, f.anna, "call.ended")
	f.setCode(404)
	for i := 0; i < webhook.FailLimit; i++ {
		f.s.Emit(ctx, f.anna, "call.ended", map[string]any{})
		f.d.Once(ctx)
		got, _ := f.s.Get(ctx, f.anna, h.ID)
		if i < webhook.FailLimit-1 && (!got.Enabled || got.Failures != i+1) {
			t.Fatalf("after %d failures: %+v", i+1, got)
		}
	}
	got, _ := f.s.Get(ctx, f.anna, h.ID)
	if got.Enabled || !strings.Contains(got.DisabledReason, "Switched off after 10") || !strings.Contains(got.DisabledReason, "404") {
		t.Errorf("after %d failures: %+v", webhook.FailLimit, got)
	}
	// switched off: nothing is queued for it any more, and what was waiting is not posted
	f.setCode(200)
	if n, _ := f.s.Emit(ctx, f.anna, "call.ended", map[string]any{}); n != 0 {
		t.Errorf("queued %d for a webhook that is off", n)
	}
	before := len(f.received())
	f.s.Pool.Exec(ctx, "INSERT INTO webhook_delivery (webhook_id, event, payload, next_at) VALUES ($1, 'call.ended', '{}', $2)", h.ID, f.now)
	f.d.Once(ctx)
	if len(f.received()) != before {
		t.Error("a delivery was posted to a webhook that is off")
	}
	// the test event goes anyway: that is how one finds out that it works again
	if err := f.s.Ping(ctx, f.anna, h.ID); err != nil {
		t.Fatal(err)
	}
	f.d.Once(ctx)
	if got := f.received(); len(got) != before+1 || got[before].header.Get("X-OpenDECTcell-Event") != "ping" || !strings.Contains(string(got[before].body), "test") {
		t.Errorf("the test event: %d posts", len(got))
	}
	if got, _ := f.s.Get(ctx, f.anna, h.ID); got.Enabled || got.LastStatus != "the test was delivered" {
		t.Errorf("a test switched it on or did not say so: %+v", got)
	}
	if err := f.s.Ping(ctx, f.ben, h.ID); !errors.Is(err, webhook.ErrNotFound) {
		t.Errorf("ben pings anna's: %v", err)
	}
}

// The controller never posts to its own surroundings, whatever a user writes: not by address, not by a name that resolves there.
func TestThePolicyIsEnforcedWhenConnecting(t *testing.T) {
	f := newFixture(t)
	h, _ := f.hook(t, f.anna, "call.ended")
	f.d.Client = webhook.NewClient(netguard.Policy{}) // the real policy: the receiver is on loopback
	f.s.Emit(ctx, f.anna, "call.ended", map[string]any{})
	f.d.Once(ctx)
	ds, _ := f.s.Deliveries(ctx, f.anna, h.ID, 1)
	if len(f.received()) != 0 || ds[0].State != "failed" || ds[0].Attempts != 1 || !strings.Contains(ds[0].Error, "does not connect") {
		t.Errorf("a post to loopback: %d posts, %+v", len(f.received()), ds[0])
	}
	if strings.Contains(ds[0].Error, "secret-path-token") || strings.Contains(ds[0].Error, f.srv.URL) {
		t.Errorf("the address is in the log: %q", ds[0].Error)
	}
}

func TestARedirectIsNotFollowed(t *testing.T) {
	f := newFixture(t)
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("the redirect was followed") }))
	defer inner.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, inner.URL, http.StatusFound) }))
	defer redir.Close()
	h, _, _ := f.s.Create(ctx, f.anna, webhook.Input{Name: "r", URL: redir.URL, Events: []string{"call.ended"}, Enabled: true})
	f.s.Emit(ctx, f.anna, "call.ended", map[string]any{})
	f.d.Once(ctx)
	ds, _ := f.s.Deliveries(ctx, f.anna, h.ID, 1)
	if ds[0].State != "failed" || ds[0].Code == nil || *ds[0].Code != 302 {
		t.Errorf("%+v", ds[0])
	}
}

func TestAnUnreachableReceiverIsRetriedWithoutTheAddress(t *testing.T) {
	f := newFixture(t)
	h, _, _ := f.s.Create(ctx, f.anna, webhook.Input{Name: "gone", URL: "http://127.0.0.1:1/hook/token-in-the-path", Events: []string{"call.ended"}, Enabled: true})
	f.s.Emit(ctx, f.anna, "call.ended", map[string]any{})
	f.d.Once(ctx)
	ds, _ := f.s.Deliveries(ctx, f.anna, h.ID, 1)
	if ds[0].State != "pending" || ds[0].Attempts != 1 || ds[0].Error == "" || strings.Contains(ds[0].Error, "token-in-the-path") || strings.Contains(ds[0].Error, "127.0.0.1:1/hook") {
		t.Errorf("%+v", ds[0])
	}
}

func TestQueueIsBoundedAndPruned(t *testing.T) {
	f := newFixture(t)
	h, _ := f.hook(t, f.anna, "call.ended")
	for i := 0; i < webhook.MaxPending+25; i++ {
		f.s.Emit(ctx, f.anna, "call.ended", map[string]any{"n": i})
	}
	var n int
	var oldest string
	f.s.Pool.QueryRow(ctx, "SELECT count(*), min((payload->'data'->>'n')::int)::text FROM webhook_delivery WHERE webhook_id = $1 AND state = 'pending'", h.ID).Scan(&n, &oldest)
	if n != webhook.MaxPending || oldest != "25" {
		t.Errorf("%d waiting, the oldest is %s: want %d and 25", n, oldest, webhook.MaxPending)
	}
	// finished ones are kept a week
	f.s.Pool.Exec(ctx, "UPDATE webhook_delivery SET state = 'done', done_at = $1 WHERE id % 2 = 0", f.now.AddDate(0, 0, -8))
	f.s.Pool.Exec(ctx, "UPDATE webhook_delivery SET state = 'failed', done_at = $1 WHERE id % 2 = 1", f.now.AddDate(0, 0, -6))
	gone, err := f.s.Prune(ctx, f.now)
	if err != nil || gone == 0 {
		t.Fatalf("prune: %d %v", gone, err)
	}
	f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_delivery WHERE done_at < $1", f.now.AddDate(0, 0, -7)).Scan(&n)
	if n != 0 {
		t.Errorf("%d old deliveries left", n)
	}
	f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_delivery").Scan(&n)
	if n == 0 {
		t.Error("everything was pruned, also what is newer than a week")
	}
	// deleting the user deletes everything
	f.s.Pool.Exec(ctx, "DELETE FROM app_user WHERE id = $1", f.anna)
	f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_delivery").Scan(&n)
	if n != 0 {
		t.Errorf("%d deliveries of a deleted user", n)
	}
}

// Two dispatchers (two controllers) do not post the same delivery.
func TestADeliveryIsPostedOnce(t *testing.T) {
	f := newFixture(t)
	f.hook(t, f.anna, "call.ended")
	f.s.Emit(ctx, f.anna, "call.ended", map[string]any{})
	d2 := &webhook.Dispatcher{Store: f.s, Client: f.d.Client}
	var wg sync.WaitGroup
	for _, d := range []*webhook.Dispatcher{f.d, d2, f.d, d2} {
		wg.Add(1)
		go func() { defer wg.Done(); d.Once(ctx) }()
	}
	wg.Wait()
	if n := len(f.received()); n != 1 {
		t.Errorf("posted %d times", n)
	}
}

// A test that fails says so, and does not count towards switching the webhook off.
func TestAFailedTestIsNotAFailure(t *testing.T) {
	f := newFixture(t)
	h, _ := f.hook(t, f.anna, "call.ended")
	f.setCode(404)
	for i := 0; i < webhook.FailLimit+2; i++ {
		if err := f.s.Ping(ctx, f.anna, h.ID); err != nil {
			t.Fatal(err)
		}
		f.d.Once(ctx)
	}
	got, _ := f.s.Get(ctx, f.anna, h.ID)
	if got.Failures != 0 || !got.Enabled || !strings.HasPrefix(got.LastStatus, "failed:") || !strings.Contains(got.LastStatus, "404") {
		t.Errorf("after %d failed tests: %+v", webhook.FailLimit+2, got)
	}
}
