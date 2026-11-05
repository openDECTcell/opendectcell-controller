// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/alerts"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
)

// LineDownAfter is how long a VoIP line may be unable to register before its owner is told.
var LineDownAfter = 10 * time.Minute

// certWarnDays is how long before it ends the certificate authority of the base stations is reported.
const certWarnDays = 90

// AlertSources are what the notification centre looks at: VoIP lines that cannot register, handsets nobody has heard from for as long
// as their owner chose, and certificates that end soon. Base stations that go offline are told by RunCellAlerts at the moment it happens.
func (s *Server) AlertSources() []alerts.Source {
	var out []alerts.Source
	if s.SIP != nil {
		out = append(out, alerts.Source{Kinds: []string{alerts.KindLine}, Find: s.lineConditions})
	}
	out = append(out, alerts.Source{Kinds: []string{alerts.KindHandset}, Find: s.handsetConditions},
		alerts.Source{Kinds: []string{alerts.KindCert}, Find: s.certConditions},
		alerts.Source{Kinds: []string{alerts.KindBackup}, Find: s.backupConditions})
	return out
}

func (s *Server) lineConditions(ctx context.Context, _ time.Time) ([]alerts.Condition, error) {
	rows, err := s.Pool.Query(ctx, "SELECT id, owner_user_id, label FROM sip_account WHERE enabled AND register")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alerts.Condition
	for rows.Next() {
		var id uuid.UUID
		var owner *uuid.UUID
		var label string
		if err := rows.Scan(&id, &owner, &label); err != nil {
			return nil, err
		}
		st, ok := s.SIP.Status(id)
		switch {
		case !ok || st.State == sip.RegRegistering:
			out = append(out, alerts.Condition{Status: alerts.Unsure, Kind: alerts.KindLine, Object: id.String()}) // not running yet, or trying
			continue
		case st.State == sip.RegRegistered:
			out = append(out, alerts.Condition{Status: alerts.Fine, Kind: alerts.KindLine, Object: id.String()})
			continue
		case st.State != sip.RegFailed && st.State != sip.RegBackoff:
			continue // not registering on purpose: not watched
		}
		who := alerts.Who{Admins: true} // a shared line is the administrators'
		if owner != nil {
			who = alerts.Who{Users: []uuid.UUID{*owner}}
		}
		body := "It cannot register"
		if st.LastError != "" {
			body += ": " + st.LastError
		}
		out = append(out, alerts.Condition{Kind: alerts.KindLine, Object: id.String(), Who: who, After: LineDownAfter, Level: "danger",
			Title: fmt.Sprintf("The VoIP line %s cannot register", label), Body: body + ". Calls on it do not work.", Href: "/sip-accounts/" + id.String(),
			OkTitle: fmt.Sprintf("The VoIP line %s is registered again", label)})
	}
	return out, rows.Err()
}

func (s *Server) handsetConditions(ctx context.Context, now time.Time) ([]alerts.Condition, error) {
	rows, err := s.Pool.Query(ctx, `SELECT h.id, h.owner_user_id, coalesce(nullif(h.display_name, ''), 'without a name'), coalesce(h.last_seen_at, h.registered_at),
			(u.alert_prefs->>'handset_hours')::int
		FROM handset h JOIN app_user u ON u.id = h.owner_user_id
		WHERE NOT u.disabled AND coalesce((u.alert_prefs->>'handset_hours')::int, 0) > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alerts.Condition
	for rows.Next() {
		var id, owner uuid.UUID
		var name string
		var seen time.Time
		var hours int
		if err := rows.Scan(&id, &owner, &name, &seen, &hours); err != nil {
			return nil, err
		}
		if seen.After(now.Add(-time.Duration(hours) * time.Hour)) {
			out = append(out, alerts.Condition{Status: alerts.Fine, Kind: alerts.KindHandset, Object: id.String()})
			continue
		}
		out = append(out, alerts.Condition{Kind: alerts.KindHandset, Object: id.String(), Who: alerts.Who{Users: []uuid.UUID{owner}}, Since: seen, After: time.Duration(hours) * time.Hour,
			Level: "warning", Title: fmt.Sprintf("The handset %s has not been heard for %s", name, ago(now, seen)),
			Body: "Last heard " + seen.Local().Format("2006-01-02 15:04") + ". It may be switched off, out of range or have an empty battery.", Href: "/handsets/" + id.String(),
			OkTitle: fmt.Sprintf("The handset %s was heard again", name)})
	}
	return out, rows.Err()
}

func (s *Server) certConditions(ctx context.Context, now time.Time) ([]alerts.Condition, error) {
	var out []alerts.Condition
	if s.Stats != nil {
		f, err := s.Stats.Fleet(ctx)
		if err != nil {
			return nil, err
		}
		if f.CertsExpiring == 0 {
			out = append(out, alerts.Condition{Status: alerts.Fine, Kind: alerts.KindCert, Object: "cell-certificates"})
		} else {
			out = append(out, alerts.Condition{Kind: alerts.KindCert, Object: "cell-certificates", Who: alerts.Who{Admins: true}, Level: "danger",
				Title: fmt.Sprintf("%d base station certificates end within 14 days and could not renew", f.CertsExpiring),
				Body:  "A base station whose certificate ends can no longer connect. Look at why it does not renew.", Href: "/cells", OkTitle: "No base station certificate ends soon any more"})
		}
	}
	var end *time.Time
	if err := s.Pool.QueryRow(ctx, "SELECT max(not_after) FROM cell_ca WHERE active").Scan(&end); err != nil {
		return nil, err
	}
	if end != nil && !end.Before(now.AddDate(0, 0, certWarnDays)) {
		out = append(out, alerts.Condition{Status: alerts.Fine, Kind: alerts.KindCert, Object: "ca"})
	}
	if end != nil && end.Before(now.AddDate(0, 0, certWarnDays)) {
		out = append(out, alerts.Condition{Kind: alerts.KindCert, Object: "ca", Who: alerts.Who{Admins: true}, Level: "danger",
			Title: "The certificate authority of the base stations ends on " + end.Local().Format("2006-01-02"),
			Body:  "When it ends, no base station can connect: every one has to be enrolled again. See the runbook in the worklog (E6).", Href: "/admin/settings", OkTitle: "The certificate authority was renewed"})
	}
	return out, nil
}

// tellCell puts a base station going offline, or coming back, in the notification centre of its operator and of the administrators.
func (s *Server) tellCell(ctx context.Context, cell uuid.UUID, level, subject, body string) {
	who := alerts.Who{Admins: true}
	var owner *uuid.UUID
	if err := s.Pool.QueryRow(ctx, "SELECT enrolled_by FROM cell WHERE id = $1", cell).Scan(&owner); err == nil && owner != nil {
		who.Users = []uuid.UUID{*owner}
	}
	if err := alerts.Notify(ctx, s.Pool, who, alerts.Notification{Kind: alerts.KindCell, Level: level, Subject: subject, Body: body, Href: "/stations/" + cell.String()}, s.AlertSink); err != nil {
		s.Log.Error("cell alerts: notification", "err", err)
	}
}

// --- the pages ---

func (s *Server) notifyRoutes() {
	s.mux.Handle("GET /notifications", s.requireUser(s.notificationsPage))
	s.mux.Handle("POST /notifications/read", s.requireUser(s.notificationsRead))
	s.mux.Handle("POST /account/alerts", s.requireUser(s.alertPrefsSave))
}

type notificationRow struct {
	ID      int64
	At      time.Time
	Level   string
	Kind    string
	Subject string
	Body    string
	Href    string
	Unread  bool
}

type notificationsView struct {
	Rows   []notificationRow
	Unread int
}

func (s *Server) unreadNotifications(ctx context.Context, user uuid.UUID) int {
	var n int
	_ = s.Pool.QueryRow(ctx, "SELECT count(*) FROM notification WHERE user_id = $1 AND read_at IS NULL", user).Scan(&n)
	return n
}

func (s *Server) notificationsPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	rows, err := s.Pool.Query(r.Context(), "SELECT id, at, level, kind, subject, body, href, read_at IS NULL FROM notification WHERE user_id = $1 ORDER BY at DESC, id DESC LIMIT 100", u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (notificationRow, error) {
		var n notificationRow
		err := r.Scan(&n.ID, &n.At, &n.Level, &n.Kind, &n.Subject, &n.Body, &n.Href, &n.Unread)
		return n, err
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	v := notificationsView{Rows: list}
	for _, n := range list {
		if n.Unread {
			v.Unread++
		}
	}
	s.render(w, http.StatusOK, "notifications.html", page{Title: "Notifications", User: u, Data: v, Notice: r.FormValue("done")})
}

// notificationsRead marks one notification (id) or all of the user's as read.
func (s *Server) notificationsRead(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var err error
	if id, perr := strconv.ParseInt(r.PostFormValue("id"), 10, 64); perr == nil {
		_, err = s.Pool.Exec(r.Context(), "UPDATE notification SET read_at = now() WHERE id = $1 AND user_id = $2 AND read_at IS NULL", id, u.ID)
	} else {
		_, err = s.Pool.Exec(r.Context(), "UPDATE notification SET read_at = now() WHERE user_id = $1 AND read_at IS NULL", u.ID)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

// alertKind is one line of the preferences form.
type alertKind struct {
	alerts.KindInfo
	Show, Mail bool
}

type alertPrefsView struct {
	Kinds        []alertKind
	HandsetHours int
	QuietFrom    string
	QuietTo      string
	MaxHours     int
}

// alertPrefsFor is what the preferences section of the account page shows.
func (s *Server) alertPrefsFor(ctx context.Context, u *auth.User) (alertPrefsView, error) {
	var raw []byte
	if err := s.Pool.QueryRow(ctx, "SELECT alert_prefs FROM app_user WHERE id = $1", u.ID).Scan(&raw); err != nil {
		return alertPrefsView{}, err
	}
	p := alerts.ParsePrefs(raw)
	v := alertPrefsView{HandsetHours: p.HandsetHours, QuietFrom: p.QuietFrom, QuietTo: p.QuietTo, MaxHours: alerts.MaxHandsetHours}
	for _, k := range alerts.Kinds {
		if k.AdminOnly && !u.IsAdmin() {
			continue
		}
		v.Kinds = append(v.Kinds, alertKind{KindInfo: k, Show: p.Shows(k.Kind), Mail: p.Mails(k.Kind)})
	}
	return v, nil
}

func (s *Server) alertPrefsSave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	fail := func(msg string) {
		s.render(w, http.StatusBadRequest, "account.html", page{Title: "Account", User: u, Error: msg})
	}
	p := alerts.Prefs{Kinds: map[string]alerts.KindPref{}, QuietFrom: strings.TrimSpace(r.PostFormValue("quiet_from")), QuietTo: strings.TrimSpace(r.PostFormValue("quiet_to"))}
	if !alerts.ValidClock(p.QuietFrom) || !alerts.ValidClock(p.QuietTo) {
		fail("Quiet hours are written as hours and minutes, like 22:00.")
		return
	}
	if (p.QuietFrom == "") != (p.QuietTo == "") {
		fail("Give both the start and the end of the quiet hours, or neither.")
		return
	}
	mailWanted := false
	for _, k := range alerts.Kinds {
		if k.AdminOnly && !u.IsAdmin() {
			continue
		}
		pref := alerts.KindPref{Hide: r.PostFormValue("show_"+k.Kind) != "on", Mail: r.PostFormValue("mail_"+k.Kind) == "on" && !k.MailByFlag}
		mailWanted = mailWanted || pref.Mail
		if pref.Hide || pref.Mail {
			p.Kinds[k.Kind] = pref
		}
	}
	if v := strings.TrimSpace(r.PostFormValue("handset_hours")); v != "" {
		h, err := strconv.Atoi(v)
		if err != nil || h < 0 || h > alerts.MaxHandsetHours {
			fail(fmt.Sprintf("The hours for a handset that is not heard are a number from 0 (off) to %d.", alerts.MaxHandsetHours))
			return
		}
		p.HandsetHours = h
	}
	if mailWanted && u.Email == "" {
		fail("Your account has no e-mail address. Ask an administrator to add one.")
		return
	}
	if _, err := s.Pool.Exec(r.Context(), "UPDATE app_user SET alert_prefs = $2 WHERE id = $1", u.ID, p.JSON()); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?done=Saved.", http.StatusSeeOther)
}
