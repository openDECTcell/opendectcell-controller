// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/netguard"
)

// backoff is the wait before the next attempt, after the first, second, ... failed one: six attempts over about 15 hours.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}

// NewClient is the HTTP client of the Dispatcher: it connects only where the policy allows (checked after the name is resolved), uses no
// proxy from the environment, follows no redirect (a receiver that sends the controller elsewhere is refused, not obeyed), and gives
// up after 15 seconds.
func NewClient(p netguard.Policy) *http.Client {
	d := &net.Dialer{Timeout: 8 * time.Second, Control: p.Control}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: d.DialContext, Proxy: nil, TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
			MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Dispatcher posts the queued deliveries.
type Dispatcher struct {
	Store  *Store
	Client *http.Client
	Log    *slog.Logger
	// Every is how often it looks for due deliveries (default 5 s); Concurrency how many it posts at once (default 4).
	Every       time.Duration
	Concurrency int
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// Run delivers until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	every := d.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := d.Once(ctx); err != nil && ctx.Err() == nil {
				d.log().Warn("webhooks", "err", err)
			}
		}
	}
}

type due struct {
	id       int64
	hook     uuid.UUID
	uid      uuid.UUID
	event    string
	payload  []byte
	attempts int
}

// Once takes the deliveries that are due, posts them, and records how it went. It returns how many it tried. A taken delivery is
// leased for two minutes (its next time is pushed out), so that a crash in the middle, or a second controller, does not post it twice
// at once.
func (d *Dispatcher) Once(ctx context.Context) (int, error) {
	s := d.Store
	now := s.now()
	rows, err := s.Pool.Query(ctx, `UPDATE webhook_delivery SET next_at = $1 WHERE id IN (
			SELECT id FROM webhook_delivery WHERE state = 'pending' AND next_at <= $2 ORDER BY next_at, id LIMIT 20 FOR UPDATE SKIP LOCKED)
		RETURNING id, webhook_id, uid, event, payload, attempts`, now.Add(2*time.Minute), now)
	if err != nil {
		return 0, err
	}
	batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var x due
		err := r.Scan(&x.id, &x.hook, &x.uid, &x.event, &x.payload, &x.attempts)
		return x, err
	})
	if err != nil {
		return 0, err
	}
	n := d.Concurrency
	if n <= 0 {
		n = 4
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, x := range batch {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := d.deliver(ctx, x); err != nil && ctx.Err() == nil {
				d.log().Warn("webhook delivery", "delivery", x.id, "err", err)
			}
		}()
	}
	wg.Wait()
	return len(batch), nil
}

type target struct {
	url, format string
	enabled     bool
	secret      []byte
	auth        []byte
}

func (d *Dispatcher) load(ctx context.Context, id uuid.UUID) (target, error) {
	var t target
	var encSecret, encAuth []byte
	if err := d.Store.Pool.QueryRow(ctx, "SELECT url, format, enabled, secret_enc, auth_enc FROM webhook WHERE id = $1", id).Scan(&t.url, &t.format, &t.enabled, &encSecret, &encAuth); err != nil {
		return t, err
	}
	var err error
	if t.secret, err = d.Store.Keyring.Open(loc(id, "secret_enc"), encSecret); err != nil {
		return t, fmt.Errorf("the signing secret cannot be opened: %w", err)
	}
	if encAuth != nil {
		if t.auth, err = d.Store.Keyring.Open(loc(id, "auth_enc"), encAuth); err != nil {
			return t, fmt.Errorf("the Authorization value cannot be opened: %w", err)
		}
	}
	return t, nil
}

// Sign is the signature of a body sent at time ts: the header value "t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">".
func Sign(secret []byte, ts time.Time, body []byte) string {
	m := hmac.New(sha256.New, secret)
	t := strconv.FormatInt(ts.Unix(), 10)
	m.Write([]byte(t + "."))
	m.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

func (d *Dispatcher) deliver(ctx context.Context, x due) error {
	s := d.Store
	t, err := d.load(ctx, x.hook)
	if err != nil {
		return d.finish(ctx, x, "failed", 0, errText(err, ""), true)
	}
	defer clear(t.secret)
	if !t.enabled && x.event != "ping" {
		return d.finish(ctx, x, "failed", 0, "the webhook is switched off", false)
	}
	body, contentType, extra := render(t.format, x.event, x.payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return d.finish(ctx, x, "failed", 0, errText(err, t.url), true)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "opendectcell-controller")
	req.Header.Set("X-OpenDECTcell-Event", x.event)
	req.Header.Set("X-OpenDECTcell-Delivery", x.uid.String())
	req.Header.Set("X-OpenDECTcell-Signature", Sign(t.secret, s.now(), body))
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	if t.auth != nil {
		req.Header.Set("Authorization", string(t.auth))
		clear(t.auth)
	}
	resp, err := d.client().Do(req)
	if err != nil {
		permanent := errors.Is(err, netguard.ErrBlocked)
		state := "retry"
		if permanent {
			state = "failed"
		}
		return d.finish(ctx, x, state, 0, errText(err, t.url), permanent)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		return d.finish(ctx, x, "done", code, "", false)
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return d.finish(ctx, x, "retry", code, "the receiver answered "+strconv.Itoa(code), false)
	}
	return d.finish(ctx, x, "failed", code, "the receiver answered "+strconv.Itoa(code)+", which is not retried", true)
}

func (d *Dispatcher) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return NewClient(d.Store.Policy)
}

// errText is an error as text for the log of deliveries: without the address (it may hold a secret), and short.
func errText(err error, address string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	if address != "" {
		msg = strings.ReplaceAll(msg, address, "<address>")
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return strings.ToValidUTF8(msg, "")
}

// finish records the outcome. state: done, failed, or retry (it may be tried again, or becomes failed when the attempts are used up).
// countFailure: a failed delivery counts towards switching the webhook off.
func (d *Dispatcher) finish(ctx context.Context, x due, state string, code int, msg string, countFailure bool) error {
	s := d.Store
	now := s.now()
	attempts := x.attempts + 1
	var codeArg *int
	if code != 0 {
		codeArg = &code
	}
	if state == "retry" && attempts > len(backoff) {
		state, countFailure, msg = "failed", true, msg+"; given up after "+strconv.Itoa(attempts)+" attempts"
	}
	switch state {
	case "retry":
		_, err := s.Pool.Exec(ctx, "UPDATE webhook_delivery SET attempts = $2, next_at = $3, last_error = $4, last_code = $5 WHERE id = $1", x.id, attempts, now.Add(backoff[attempts-1]), msg, codeArg)
		return err
	case "done":
		if _, err := s.Pool.Exec(ctx, "UPDATE webhook_delivery SET state = 'done', attempts = $2, done_at = $3, last_error = '', last_code = $4 WHERE id = $1", x.id, attempts, now, codeArg); err != nil {
			return err
		}
		if x.event == "ping" {
			_, err := s.Pool.Exec(ctx, "UPDATE webhook SET last_at = $2, last_status = 'the test was delivered' WHERE id = $1", x.hook, now)
			return err
		}
		_, err := s.Pool.Exec(ctx, "UPDATE webhook SET failures = 0, last_at = $2, last_status = 'delivered' WHERE id = $1", x.hook, now)
		return err
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE webhook_delivery SET state = 'failed', attempts = $2, done_at = $3, last_error = $4, last_code = $5 WHERE id = $1", x.id, attempts, now, msg, codeArg); err != nil {
		return err
	}
	if !countFailure || x.event == "ping" {
		_, err := s.Pool.Exec(ctx, "UPDATE webhook SET last_at = $2, last_status = $3 WHERE id = $1", x.hook, now, "failed: "+msg)
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE webhook SET failures = failures + 1, last_at = $2, last_status = $3,
		enabled = enabled AND failures + 1 < $4,
		disabled_reason = CASE WHEN enabled AND failures + 1 >= $4 THEN $5 ELSE disabled_reason END WHERE id = $1`,
		x.hook, now, "failed: "+msg, FailLimit, fmt.Sprintf("Switched off after %d deliveries in a row failed. The last: %s", FailLimit, msg))
	return err
}

// render makes the body and the extra headers of a post from the stored envelope.
func render(format, event string, payload []byte) (body []byte, contentType string, headers map[string]string) {
	if format != FormatNtfy {
		return payload, "application/json", nil
	}
	var env struct {
		Data struct {
			Summary string `json:"summary"`
			Message string `json:"message"`
			Subject string `json:"subject"`
			Level   string `json:"level"`
		} `json:"data"`
	}
	_ = json.Unmarshal(payload, &env)
	text := env.Data.Summary
	if text == "" {
		text = env.Data.Message
	}
	if text == "" {
		text = env.Data.Subject
	}
	title := eventTitle(event)
	headers = map[string]string{"Title": title, "Tags": event}
	if event == "call.missed" || env.Data.Level == "danger" {
		headers["Priority"] = "high"
	}
	return []byte(text), "text/plain; charset=utf-8", headers
}

func eventTitle(event string) string {
	switch event {
	case "call.incoming":
		return "Incoming call"
	case "call.missed":
		return "Missed call"
	case "call.ended":
		return "Call ended"
	case "handset.online":
		return "Handset online"
	case "handset.offline":
		return "Handset offline"
	case "notification":
		return "opendectcell"
	}
	return "openDECTcell test"
}
