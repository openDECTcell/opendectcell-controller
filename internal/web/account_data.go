// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/userdata"
)

func (s *Server) accountDataRoutes() {
	s.mux.Handle("GET /account/export", s.requireUser(s.accountExport))
	s.mux.Handle("GET /account/erase", s.requireUser(s.accountErasePage))
	s.mux.Handle("POST /account/erase", s.requireUser(s.accountErase))
}

// accountExport sends everything the controller holds about the user as a JSON file (no secrets).
func (s *Server) accountExport(w http.ResponseWriter, r *http.Request, u *auth.User) {
	b, err := userdata.Export(r.Context(), s.Pool, u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	_ = audit.Record(r.Context(), s.Pool, audit.Entry{Action: "user.export", ObjectType: "user", ObjectID: u.ID.String()})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="opendectcell-data-`+time.Now().Format("2006-01-02")+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

type eraseData struct {
	Handsets, Profiles, Cells int
	Local                     bool
}

func (s *Server) eraseCounts(r *http.Request, u *auth.User) (eraseData, error) {
	d := eraseData{Local: u.AuthSource == "local"}
	if s.Handsets != nil {
		hs, err := s.Handsets.List(r.Context(), u)
		if err != nil {
			return d, err
		}
		d.Handsets = len(hs)
	}
	if err := s.Pool.QueryRow(r.Context(), "SELECT count(*) FROM sip_account WHERE owner_user_id = $1", u.ID).Scan(&d.Profiles); err != nil {
		return d, err
	}
	err := s.Pool.QueryRow(r.Context(), "SELECT count(*) FROM cell WHERE enrolled_by = $1 AND state <> 'revoked'", u.ID).Scan(&d.Cells)
	return d, err
}

func (s *Server) accountErasePage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	d, err := s.eraseCounts(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "account_erase.html", page{Title: "Delete my account and data", User: u, Data: d})
}

// accountErase deletes the account with everything it owns: handsets (the cells are told to forget them), VoIP profiles, base stations
// (revoked), contacts, call history; the audit log forgets who the user was. It needs the password (single sign-on: the user name typed).
func (s *Server) accountErase(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d, err := s.eraseCounts(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	fail := func(status int, msg string) {
		s.render(w, status, "account_erase.html", page{Title: "Delete my account and data", User: u, Data: d, Error: msg})
	}
	if r.PostFormValue("understand") != "on" {
		fail(http.StatusBadRequest, "Tick the box to say you understand that this cannot be undone.")
		return
	}
	if u.AuthSource == "local" {
		if err := s.Auth.CheckPassword(r.Context(), u.ID, r.PostFormValue("password"), addrFrom(r.Context()).String()); err != nil {
			msg := "The password is wrong."
			if errors.Is(err, auth.ErrRateLimited) {
				msg = "Too many failed attempts. Try again in a few minutes."
			}
			fail(http.StatusBadRequest, msg)
			return
		}
	} else if !strings.EqualFold(strings.TrimSpace(r.PostFormValue("username")), u.Username) {
		fail(http.StatusBadRequest, "Type your user name to confirm.")
		return
	}
	ctx := r.Context()
	// the administrator who is the last one must not erase themselves: find out before anything is removed
	if u.IsAdmin() || u.Admin2FAPending {
		var others int
		if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM app_user WHERE role = 'admin' AND id <> $1 AND NOT disabled", u.ID).Scan(&others); err != nil {
			s.internalError(w, r, err)
			return
		}
		if others == 0 {
			fail(http.StatusConflict, userdata.ErrLastAdmin.Error()+".")
			return
		}
	}
	// handsets first: the cells are told to forget each one (without asking it: it may be out of range), then its rows go
	if s.Handsets != nil {
		hs, err := s.Handsets.List(ctx, u)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		for _, h := range hs {
			if s.HandsetDeregister != nil {
				s.HandsetDeregister(h.ID, true)
			}
			if err := s.Handsets.Delete(ctx, u, h.ID); err != nil {
				s.internalError(w, r, err)
				return
			}
		}
	}
	if s.SIPAccounts != nil {
		recs, err := s.SIPAccounts.List(ctx, u, "")
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		for _, rec := range recs {
			if rec.OwnerID != nil && *rec.OwnerID == u.ID {
				if err := s.SIPAccounts.Delete(ctx, u, rec.ID); err != nil {
					s.internalError(w, r, err)
					return
				}
			}
		}
	}
	if s.Cells != nil {
		cs, err := s.Cells.ListOwned(ctx, u)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		for _, c := range cs {
			if c.EnrolledBy == nil || *c.EnrolledBy != u.ID {
				continue
			}
			if err := s.Cells.RevokeOwned(ctx, u, c.ID); err != nil {
				s.internalError(w, r, err)
				return
			}
			if s.CellLink != nil {
				s.CellLink.Kick(c.ID)
			}
		}
	}
	if err := userdata.EraseRecords(ctx, s.Pool, u.ID); err != nil {
		if errors.Is(err, userdata.ErrLastAdmin) {
			fail(http.StatusConflict, err.Error()+".")
			return
		}
		s.internalError(w, r, err)
		return
	}
	s.clearCookie(w)
	http.Redirect(w, r, "/login?erased=1", http.StatusSeeOther)
}
