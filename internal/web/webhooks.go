// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/alerts"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/webhook"
)

// --- the events ---

// emit queues an event of a user for their webhooks, off the caller's path and with a time limit of its own: nothing that calls it waits
// for the database or may be stopped by an error of the webhooks.
func (s *Server) emit(user uuid.UUID, event string, data any) {
	if s.Webhooks == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.Log.Error("webhook event", "event", event, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := s.Webhooks.Emit(ctx, user, event, data); err != nil {
			s.Log.Warn("webhook event", "event", event, "err", err)
		}
	}()
}

type callEventData struct {
	ID              int64      `json:"id"`
	Direction       string     `json:"direction"`
	RemoteParty     string     `json:"remote_party"`
	RemoteName      string     `json:"remote_name,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	AnsweredAt      *time.Time `json:"answered_at"`
	EndedAt         *time.Time `json:"ended_at"`
	DurationSeconds int64      `json:"duration_seconds"`
	Result          string     `json:"result"`
	HandsetID       *uuid.UUID `json:"handset_id"`
	Handset         string     `json:"handset,omitempty"`
	SIPAccountID    *uuid.UUID `json:"sip_account_id"`
	Line            string     `json:"line,omitempty"`
	Summary         string     `json:"summary"`
}

// CallEvent is the hook of the call log (calllog.Store.Hook): a call that comes in is call.incoming, a call that ends is call.ended, and
// an incoming call nobody answered is also call.missed. The call is read from the log, so the event carries what the log holds.
func (s *Server) CallEvent(ctx context.Context, event string, id int64) {
	if s.Webhooks == nil || (event != calllog.EventStarted && event != calllog.EventEnded) {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.Log.Error("webhook call event", "panic", r)
			}
		}()
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		c, err := (&calllog.Store{Pool: s.Pool}).Get(cctx, id)
		if err != nil || c.OwnerID == nil {
			return
		}
		if event == calllog.EventStarted && c.Direction != "in" {
			return
		}
		d := callEventData{ID: c.ID, Direction: c.Direction, RemoteParty: c.RemoteParty, StartedAt: c.StartedAt, AnsweredAt: c.AnsweredAt, EndedAt: c.EndedAt,
			DurationSeconds: int64(c.Duration() / time.Second), HandsetID: c.HandsetID, SIPAccountID: c.SIPAccountID}
		d.Result, _ = callResult(c)
		if c.HandsetID != nil {
			_ = s.Pool.QueryRow(cctx, "SELECT display_name FROM handset WHERE id = $1", *c.HandsetID).Scan(&d.Handset)
		}
		if c.SIPAccountID != nil {
			_ = s.Pool.QueryRow(cctx, "SELECT label FROM sip_account WHERE id = $1", *c.SIPAccountID).Scan(&d.Line)
		}
		d.RemoteName = s.contactNames(cctx, *c.OwnerID, []calllog.Call{c})[c.RemoteParty]
		who := strings.TrimSpace(c.RemoteParty)
		if d.RemoteName != "" {
			who = d.RemoteName + " (" + who + ")"
		}
		if who == "" {
			who = "an unknown number"
		}
		emit := func(name, summary string) {
			d.Summary = summary
			if _, err := s.Webhooks.Emit(cctx, *c.OwnerID, name, d); err != nil {
				s.Log.Warn("webhook event", "event", name, "err", err)
			}
		}
		switch event {
		case calllog.EventStarted:
			emit("call.incoming", "Incoming call from "+who)
		default:
			emit("call.ended", "Call with "+who+" ended ("+d.Result+")")
			if c.Missed() {
				emit("call.missed", "Missed call from "+who)
			}
		}
	}()
}

// NotificationEvent is the sink of the notification centre (Server.AlertSink): what a user is told there, their webhooks get too.
func (s *Server) NotificationEvent(user uuid.UUID, n alerts.Notification) {
	s.emit(user, "notification", map[string]any{"kind": n.Kind, "level": n.Level, "subject": n.Subject, "body": n.Body, "href": n.Href, "summary": n.Subject})
}

// RunHandsetWebhooks watches the handsets of the users who have a webhook for handset.online or handset.offline, and emits the event
// when a handset's presence changes between online and not online. The first look at a handset (and a restart) says nothing: it is
// not a change.
func (s *Server) RunHandsetWebhooks(ctx context.Context) {
	if s.Webhooks == nil || s.Handsets == nil {
		return
	}
	prev := map[uuid.UUID]bool{}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkHandsetWebhooks(ctx, prev)
		}
	}
}

func (s *Server) checkHandsetWebhooks(ctx context.Context, prev map[uuid.UUID]bool) {
	rows, err := s.Pool.Query(ctx, "SELECT DISTINCT user_id FROM webhook WHERE enabled AND events && ARRAY['handset.online', 'handset.offline']")
	if err != nil {
		s.Log.Warn("handset webhooks", "err", err)
		return
	}
	var users []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			users = append(users, id)
		}
	}
	rows.Close()
	seen := map[uuid.UUID]bool{}
	for _, uid := range users {
		hs, err := s.Handsets.List(ctx, &auth.User{ID: uid, Role: auth.RoleUser})
		if err != nil {
			continue
		}
		for _, h := range hs {
			seen[h.ID] = true
			online := h.Presence == handset.Online
			if was, known := prev[h.ID]; known && was != online {
				name, word := "handset.offline", "is not heard any more"
				if online {
					name, word = "handset.online", "is heard again"
				}
				s.emit(uid, name, map[string]any{"id": h.ID, "display_name": h.Name, "presence": string(h.Presence), "summary": "The handset " + h.Name + " " + word})
			}
			prev[h.ID] = online
		}
	}
	for id := range prev {
		if !seen[id] {
			delete(prev, id) // removed, or its owner has no such webhook any more
		}
	}
}

// --- the pages ---

func (s *Server) webhookRoutes() {
	if s.Webhooks == nil {
		return
	}
	s.mux.Handle("GET /account/webhooks", s.requireUser(s.webhooksPage))
	s.mux.Handle("POST /account/webhooks", s.requireUser(s.webhookCreate))
	s.mux.Handle("GET /account/webhooks/{id}", s.requireUser(s.webhookPage))
	s.mux.Handle("POST /account/webhooks/{id}", s.requireUser(s.webhookUpdate))
	s.mux.Handle("POST /account/webhooks/{id}/rotate", s.requireUser(s.webhookRotate))
	s.mux.Handle("POST /account/webhooks/{id}/test", s.requireUser(s.webhookTest))
	s.mux.Handle("POST /account/webhooks/{id}/delete", s.requireUser(s.webhookDelete))
}

type hookRow struct {
	webhook.Hook
	Host string
}

type webhooksView struct {
	Hooks     []hookRow
	Events    []webhook.Event
	Local     bool
	NewSecret string
	NewName   string
	Form      webhookForm
	Max       int
	Insecure  bool // plain http is allowed here (private networks are)
}

type webhookForm struct {
	Name, URL, Format string
	Events            map[string]bool
	Enabled           bool
	HasAuth           bool
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

func (s *Server) webhooksView(r *http.Request, u *auth.User) (webhooksView, error) {
	hs, err := s.Webhooks.List(r.Context(), u.ID)
	if err != nil {
		return webhooksView{}, err
	}
	v := webhooksView{Events: webhook.Events, Local: u.AuthSource == "local", Max: webhook.MaxPerUser, Insecure: s.Webhooks.Policy.AllowPrivate,
		Form: webhookForm{Format: webhook.FormatJSON, Events: map[string]bool{}, Enabled: true}}
	for _, h := range hs {
		v.Hooks = append(v.Hooks, hookRow{Hook: h, Host: hostOf(h.URL)})
	}
	return v, nil
}

func (s *Server) webhooksPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	v, err := s.webhooksView(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "account_webhooks.html", page{Title: "Webhooks", User: u, Data: v, Notice: r.FormValue("done")})
}

// webhookInput reads the form of a webhook.
func webhookInput(r *http.Request) (webhook.Input, webhookForm) {
	in := webhook.Input{Name: r.PostFormValue("name"), URL: r.PostFormValue("url"), Format: r.PostFormValue("format"), Enabled: r.PostFormValue("enabled") == "on",
		Auth: strings.TrimSpace(r.PostFormValue("auth")), ClearAuth: r.PostFormValue("clear_auth") == "on"}
	form := webhookForm{Name: in.Name, URL: in.URL, Format: in.Format, Events: map[string]bool{}, Enabled: in.Enabled}
	for _, e := range webhook.Events {
		if r.PostFormValue("event_"+e.Name) == "on" {
			in.Events = append(in.Events, e.Name)
			form.Events[e.Name] = true
		}
	}
	return in, form
}

func (s *Server) webhookCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	v, err := s.webhooksView(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	in, form := webhookInput(r)
	v.Form = form
	fail := func(status int, msg string) {
		s.render(w, status, "account_webhooks.html", page{Title: "Webhooks", User: u, Data: v, Error: msg})
	}
	if msg := s.confirmUser(r, u); msg != "" {
		fail(http.StatusBadRequest, msg)
		return
	}
	h, sec, err := s.Webhooks.Create(r.Context(), u.ID, in)
	var verr webhook.ValidationError
	switch {
	case errors.As(err, &verr):
		fail(http.StatusBadRequest, verr.Error())
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	if v, err = s.webhooksView(r, u); err != nil {
		s.internalError(w, r, err)
		return
	}
	v.NewSecret, v.NewName = sec, h.Name
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "account_webhooks.html", page{Title: "Webhooks", User: u, Data: v})
}

type webhookDetail struct {
	Hook       webhook.Hook
	Events     []webhook.Event
	Selected   map[string]bool
	Deliveries []webhook.Delivery
	Local      bool
	NewSecret  string
	Insecure   bool
}

func (s *Server) webhookDetail(r *http.Request, u *auth.User, id uuid.UUID) (webhookDetail, error) {
	h, err := s.Webhooks.Get(r.Context(), u.ID, id)
	if err != nil {
		return webhookDetail{}, err
	}
	d := webhookDetail{Hook: h, Events: webhook.Events, Selected: map[string]bool{}, Local: u.AuthSource == "local", Insecure: s.Webhooks.Policy.AllowPrivate}
	for _, e := range h.Events {
		d.Selected[e] = true
	}
	d.Deliveries, err = s.Webhooks.Deliveries(r.Context(), u.ID, id, 20)
	return d, err
}

func (s *Server) hookByPath(w http.ResponseWriter, r *http.Request, u *auth.User) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, u)
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) webhookPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.hookByPath(w, r, u)
	if !ok {
		return
	}
	d, err := s.webhookDetail(r, u, id)
	switch {
	case errors.Is(err, webhook.ErrNotFound):
		s.notFound(w, r, u)
	case err != nil:
		s.internalError(w, r, err)
	default:
		s.render(w, http.StatusOK, "account_webhook.html", page{Title: d.Hook.Name, User: u, Data: d, Notice: r.FormValue("done")})
	}
}

func (s *Server) webhookUpdate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	id, ok := s.hookByPath(w, r, u)
	if !ok {
		return
	}
	d, err := s.webhookDetail(r, u, id)
	if errors.Is(err, webhook.ErrNotFound) {
		s.notFound(w, r, u)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	in, form := webhookInput(r)
	d.Hook.Name, d.Hook.URL, d.Hook.Format, d.Hook.Enabled, d.Selected = in.Name, in.URL, in.Format, in.Enabled, form.Events
	fail := func(status int, msg string) {
		s.render(w, status, "account_webhook.html", page{Title: d.Hook.Name, User: u, Data: d, Error: msg})
	}
	if msg := s.confirmUser(r, u); msg != "" {
		fail(http.StatusBadRequest, msg)
		return
	}
	_, err = s.Webhooks.Update(r.Context(), u.ID, id, in)
	var verr webhook.ValidationError
	switch {
	case errors.As(err, &verr):
		fail(http.StatusBadRequest, verr.Error())
	case errors.Is(err, webhook.ErrNotFound):
		s.notFound(w, r, u)
	case err != nil:
		s.internalError(w, r, err)
	default:
		http.Redirect(w, r, "/account/webhooks/"+id.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: a UUID we parsed
	}
}

func (s *Server) webhookRotate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	id, ok := s.hookByPath(w, r, u)
	if !ok {
		return
	}
	d, err := s.webhookDetail(r, u, id)
	if errors.Is(err, webhook.ErrNotFound) {
		s.notFound(w, r, u)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if msg := s.confirmUser(r, u); msg != "" {
		s.render(w, http.StatusBadRequest, "account_webhook.html", page{Title: d.Hook.Name, User: u, Data: d, Error: msg})
		return
	}
	sec, err := s.Webhooks.RotateSecret(r.Context(), u.ID, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	d.NewSecret = sec
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "account_webhook.html", page{Title: d.Hook.Name, User: u, Data: d})
}

func (s *Server) webhookTest(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.hookByPath(w, r, u)
	if !ok {
		return
	}
	switch err := s.Webhooks.Ping(r.Context(), u.ID, id); {
	case errors.Is(err, webhook.ErrNotFound):
		s.notFound(w, r, u)
	case err != nil:
		s.internalError(w, r, err)
	default:
		http.Redirect(w, r, "/account/webhooks/"+id.String()+"?done=A+test+event+is+on+its+way.+It+is+sent+within+seconds;+reload+this+page+to+see+how+it+went.", http.StatusSeeOther) //nolint:gosec // G710: a UUID we parsed
	}
}

func (s *Server) webhookDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id, ok := s.hookByPath(w, r, u)
	if !ok {
		return
	}
	switch err := s.Webhooks.Delete(r.Context(), u.ID, id); {
	case errors.Is(err, webhook.ErrNotFound):
		s.notFound(w, r, u)
	case err != nil:
		s.internalError(w, r, err)
	default:
		http.Redirect(w, r, "/account/webhooks?done=Webhook+removed.", http.StatusSeeOther)
	}
}
