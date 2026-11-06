// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
)

func (s *Server) activityRoutes() {
	s.mux.Handle("GET /account/activity", s.requireUser(s.activityPage))
}

// actionWords is what an action of the audit log says to a person. An action that is not here is shown as its name with the dots and
// underscores read as spaces.
var actionWords = map[string]string{ //nolint:gosec // G101: sentences about passwords, not credentials
	"user.login": "Signed in", "user.logout": "Signed out", "user.login_failed": "A failed attempt to sign in", "user.login_ratelimited": "Sign-in attempts were refused: too many failures",
	"user.login_denied": "A sign-in was refused", "user.password_change": "Password changed", "user.password_reset": "Password reset by e-mail", "user.password_check_failed": "A wrong password was typed to confirm something",
	"user.totp_on": "Two-factor login switched on", "user.totp_off": "Two-factor login switched off", "user.totp_failed": "A wrong two-factor code", "user.recovery_code_used": "A recovery code was used",
	"user.devices_forgotten": "Remembered devices were forgotten", "session.end": "A session was ended", "session.end_others": "Your other sessions were ended",
	"user.update": "The account was changed", "user.role_change": "The role of the account was changed", "user.trust": "The trust setting was changed", "user.export": "Your data was downloaded", "user.register": "Account made",
	"user.email_verified": "E-mail address confirmed",
	"handset.update":      "A handset was changed", "handset.line": "A handset's VoIP line was changed", "handset.delete": "A handset was removed",
	"sip_account.create": "A VoIP line was made", "sip_account.update": "A VoIP line was changed", "sip_account.delete": "A VoIP line was deleted",
	"contact.create": "A contact was made", "contact.update": "A contact was changed", "contact.delete": "A contact was deleted",
	"addressbook.create": "An address book was linked", "addressbook.update": "An address book was changed", "addressbook.delete": "An address book was unlinked",
	"registration_window.open": "A registration window was opened", "registration_window.cancel": "A registration window was closed",
	"cell.enroll": "A base station was enrolled", "cell.update": "A base station was changed", "cell.revoke": "A base station was revoked", "cell.purge": "A revoked base station was deleted", "cell.profile": "A base station's profile was changed",
	"cell_token.create": "An enrolment token was made", "cell_token.delete": "An enrolment token was deleted",
	"org.create": "An organization was made", "org.join": "An organization was joined", "org.leave": "An organization was left", "org.invite": "Someone was invited to an organization",
	"org.rename": "An organization was renamed", "org.role": "A role in an organization was changed", "org.remove_member": "A member was removed from an organization", "org.delete": "An organization was deleted",
	"api_token.create": "An API token was made", "api_token.revoke": "An API token was revoked",
	"webhook.create": "A webhook was made", "webhook.update": "A webhook was changed", "webhook.rotate": "A webhook's signing secret was renewed", "webhook.delete": "A webhook was removed",
	"setup.export": "Your setup was exported", "setup.import": "A setup file was imported",
}

type activityRow struct {
	ID      int64
	At      time.Time
	Text    string
	Detail  string
	From    string // the address, if there is one
	ByOther string // who did it, when it was not the user
	Warn    bool   // a failed or refused attempt
}

type activityView struct {
	Rows    []activityRow
	Kind    string
	Next    int64
	NextURL string
}

// detailText is the harmless details of an entry as "key: value" (the audit log keeps no secrets by design), at most four, in key order.
func detailText(d map[string]any) string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch v := d[k].(type) {
		case string:
			if v != "" {
				parts = append(parts, k+": "+v)
			}
		case float64:
			parts = append(parts, k+": "+strconv.FormatFloat(v, 'f', -1, 64))
		case bool:
			parts = append(parts, k+": "+strconv.FormatBool(v))
		}
		if len(parts) == 4 {
			break
		}
	}
	return strings.Join(parts, ", ")
}

func (s *Server) activityPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	kind := r.FormValue("kind")
	if kind != "access" && kind != "changes" {
		kind = ""
	}
	before, _ := strconv.ParseInt(r.FormValue("before"), 10, 64)
	const pageSize = 50
	rows, err := audit.ListForUser(r.Context(), s.Pool, audit.ForUser{ID: u.ID, Username: u.Username, Kind: kind, Before: before, Limit: pageSize})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	v := activityView{Kind: kind}
	for _, e := range rows {
		text := actionWords[e.Action]
		if text == "" {
			text = strings.NewReplacer(".", " ", "_", " ").Replace(e.Action)
		}
		row := activityRow{ID: e.ID, At: e.At, Text: text, Detail: detailText(e.Details),
			Warn: strings.HasPrefix(e.Action, "user.login_") || e.Action == "user.totp_failed" || e.Action == "user.password_check_failed"}
		if e.RemoteAddr != nil {
			row.From = e.RemoteAddr.String()
		}
		if e.ActorID == nil || *e.ActorID != u.ID {
			switch e.ActorName {
			case "", "anonymous":
			case "system":
				row.ByOther = "the controller"
			default:
				row.ByOther = e.ActorName
			}
		}
		v.Rows = append(v.Rows, row)
	}
	if len(rows) == pageSize {
		v.Next = rows[len(rows)-1].ID
		v.NextURL = fmt.Sprintf("/account/activity?before=%d&kind=%s", v.Next, kind)
	}
	s.render(w, http.StatusOK, "account_activity.html", page{Title: "My activity", User: u, Data: v})
}
