// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"

	"github.com/openDECTcell/opendectcell-controller/internal/sip/resolve"
)

// Message waiting (RFC 3842). An account with MWI on subscribes to its
// mailbox's message-summary after it has registered, keeps the subscription up,
// and reports what the NOTIFYs say to Config.OnMWI. A server that pushes
// NOTIFYs without being asked is heard too (lineFor finds the account).

// MWI is what a mailbox says about its messages.
type MWI struct {
	// Waiting: there are new messages.
	Waiting bool
	// New is the number of new voice messages; 1 when the server says "yes" and
	// gives no count.
	New uint
}

// ParseMessageSummary reads the body of a message-summary NOTIFY
// (application/simple-message-summary).
func ParseMessageSummary(body []byte) MWI {
	var out MWI
	counted := false
	for _, ln := range strings.Split(string(body), "\n") {
		name, val, ok := strings.Cut(strings.TrimSpace(ln), ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "messages-waiting":
			out.Waiting = strings.EqualFold(val, "yes")
		case "voice-message":
			// "2/8 (0/1)": new/old (urgent new/urgent old)
			n, _, _ := strings.Cut(val, "/")
			if v, err := strconv.ParseUint(strings.TrimSpace(n), 10, 16); err == nil {
				out.New, counted = uint(v), true
			}
		}
	}
	switch {
	case counted:
		out.Waiting = out.New > 0
	case out.Waiting:
		out.New = 1
	}
	return out
}

// mwiRetry is how long to wait after a subscription failed: the server may not
// support it (it can still push NOTIFYs), or be down.
const (
	mwiRetry     = 10 * time.Minute
	mwiExpiry    = 3600
	mwiMinExpiry = 60
)

// startMWI begins the account's subscription, once per line.
func (l *line) startMWI() {
	if !l.acct.MWI {
		return
	}
	l.mwiOnce.Do(func() { go l.mwiLoop() })
}

func (l *line) mwiLoop() {
	for l.ctx.Err() == nil {
		wait := mwiRetry
		l.mu.Lock()
		t, ok := l.target, l.hasTgt
		l.mu.Unlock()
		if ok {
			granted, err := l.subscribe(l.ctx, t, mwiExpiry)
			if err == nil {
				wait = max(refreshAfter(granted), mwiMinExpiry*time.Second/2)
			} else {
				l.m.log.Info("SIP message-waiting subscription failed", "account", l.acct.ID, "err", err)
			}
		}
		select {
		case <-l.ctx.Done():
			return
		case <-l.mwiKick:
		case <-time.After(wait):
		}
	}
}

// subscribe sends one SUBSCRIBE (a refresh when the dialog exists, answering
// one challenge) and returns the granted expiry.
func (l *line) subscribe(ctx context.Context, t resolve.Target, expires int) (time.Duration, error) {
	l.mu.Lock()
	cseq := l.mwiCSeq
	l.mwiCSeq++
	callID, tag, toTag := l.mwiCallID, l.mwiFromTag, l.mwiToTag
	l.mu.Unlock()
	req := sip.NewRequest(sip.SUBSCRIBE, l.aor())
	l.baseHeaders(req, t, sip.SUBSCRIBE, callID, tag, cseq)
	to := &sip.ToHeader{Address: l.aor(), Params: sip.NewParams()}
	if toTag != "" {
		to.Params.Add("tag", toTag)
	}
	req.AppendHeader(to)
	req.AppendHeader(l.contact(t))
	req.AppendHeader(sip.NewHeader("Event", "message-summary"))
	req.AppendHeader(sip.NewHeader("Accept", "application/simple-message-summary"))
	exp := sip.ExpiresHeader(expires)
	req.AppendHeader(&exp)
	var res *sip.Response
	var err error
	for attempt := 0; ; attempt++ {
		if res, err = l.do(ctx, req); err != nil {
			return 0, err
		}
		if (res.StatusCode != 401 && res.StatusCode != 407) || attempt >= 2 || l.acct.Password == "" {
			break
		}
		if attempt == 1 && !staleNonce(res) {
			break
		}
		req = req.Clone()
		if err := applyDigest(req, res, l.acct.authUser(), l.acct.Password); err != nil {
			return 0, err
		}
		req.CSeq().SeqNo++
		l.mu.Lock()
		l.mwiCSeq = max(l.mwiCSeq, req.CSeq().SeqNo+1)
		l.mu.Unlock()
		req.RemoveHeader("Via")
		req.PrependHeader(l.m.via(t))
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return 0, &responseError{res.StatusCode, res.Reason}
	}
	if rt := res.To(); rt != nil {
		if tag, ok := rt.Params.Get("tag"); ok {
			l.mu.Lock()
			l.mwiToTag = tag
			l.mu.Unlock()
		}
	}
	granted := time.Duration(expires) * time.Second
	if h := res.GetHeader("Expires"); h != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Value())); err == nil && n > 0 {
			granted = time.Duration(n) * time.Second
		}
	}
	return granted, nil
}

// onNotify answers a NOTIFY: a message-summary updates the account's
// mailbox; any other event is accepted and ignored.
func (m *Manager) onNotify(req *sip.Request, tx sip.ServerTransaction) {
	ev := ""
	if h := req.GetHeader("Event"); h != nil {
		ev, _, _ = strings.Cut(h.Value(), ";")
	}
	if !strings.EqualFold(strings.TrimSpace(ev), "message-summary") {
		m.respond(tx, req, 200, "OK")
		return
	}
	l := m.lineFor(req)
	if l == nil {
		m.respond(tx, req, 481, "Subscription Does Not Exist")
		return
	}
	m.respond(tx, req, 200, "OK")
	if st := req.GetHeader("Subscription-State"); st != nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(st.Value())), "terminated") {
		// the server ended it: subscribe again at the next turn
		select {
		case l.mwiKick <- struct{}{}:
		default:
		}
	}
	body := req.Body()
	if len(body) == 0 {
		return
	}
	if m.cfg.OnMWI != nil && l.acct.MWI {
		m.cfg.OnMWI(l.acct.ID, ParseMessageSummary(body))
	}
}
