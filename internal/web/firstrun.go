// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

// The first-run step of a new controller: the DECT network identity. Several base stations are one DECT system, with
// handsets that register once and move between them, only when they share an identity; without one every box keeps its
// factory identity and is a system of its own. That is the point of the controller, so the first administrator is
// proposed one right after signing in, while no handset is registered and nothing can break. They may decline (and the
// page says what that means); the overview then keeps a warning until an identity is set on the Network identity page.
func (s *Server) firstRunRoutes() {
	if s.Cells == nil || s.Settings == nil {
		return
	}
	s.mux.Handle("GET /setup/identity", s.requireAdmin(s.setupIdentityPage))
	s.mux.Handle("POST /setup/identity", s.requireAdmin(s.setupIdentityPost))
}

// identityStepDue reports whether the first-run identity step is to be offered: there is no system identity, nobody
// declined one, and no handset is registered yet (with handsets about, setting an identity makes each of them register
// again, which the Network identity page asks about; the step does not).
func (s *Server) identityStepDue(ctx context.Context) (bool, error) {
	if s.Cells == nil || s.Settings == nil {
		return false, nil
	}
	id, err := s.Cells.SystemIdentity(ctx)
	if err != nil || id != nil {
		return false, err
	}
	var set settings.Setup
	if _, err := s.Settings.Get(ctx, settings.KeySetup, &set); err != nil || set.IdentityDeclined {
		return false, err
	}
	var none bool
	if err := s.Pool.QueryRow(ctx, "SELECT NOT EXISTS (SELECT 1 FROM handset)").Scan(&none); err != nil {
		return false, err
	}
	return none, nil
}

// setupIdentityData is the step's page: the identity proposed.
type setupIdentityData struct {
	Proposed cell.SystemIdentity
}

func (s *Server) renderSetupIdentity(w http.ResponseWriter, u *auth.User, status int, id cell.SystemIdentity, errMsg string) {
	s.render(w, status, "setup_identity.html", page{Title: "First set-up: the DECT network", User: u, Error: errMsg, Data: setupIdentityData{Proposed: id}})
}

func (s *Server) setupIdentityPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	due, err := s.identityStepDue(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !due {
		http.Redirect(w, r, "/network-identity", http.StatusSeeOther)
		return
	}
	id, err := cell.RandomIdentity(nil)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.renderSetupIdentity(w, u, http.StatusOK, id, "")
}

// setupIdentityPost: "accept" saves the proposal shown and turns it on for the cells, "another" draws a new proposal,
// "eic" proposes the EIC typed in, "decline" says no and remembers it.
func (s *Server) setupIdentityPost(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	due, err := s.identityStepDue(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !due { // somebody else decided meanwhile, or a handset is registered: the careful page
		http.Redirect(w, r, "/network-identity", http.StatusSeeOther)
		return
	}
	switch r.PostFormValue("action") {
	case "decline":
		raw, err := json.Marshal(settings.Setup{IdentityDeclined: true, IdentityDeclinedBy: u.Username, IdentityDeclinedAt: time.Now().UTC()})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(r.Context(), `INSERT INTO setting (key, value, updated_at) VALUES ($1, $2, now())
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, settings.KeySetup, raw); err != nil {
				return err
			}
			return audit.Record(r.Context(), tx, audit.Entry{Action: "setup.identity_declined", ObjectType: "setting", ObjectID: settings.KeySetup})
		})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case "another":
		id, err := cell.RandomIdentity(nil)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		s.renderSetupIdentity(w, u, http.StatusOK, id, "")
	case "eic":
		id, err := identityForm(r, cell.IdentityAllocated)
		if err != nil {
			rnd, rerr := cell.RandomIdentity(nil)
			if rerr != nil {
				s.internalError(w, r, rerr)
				return
			}
			s.renderSetupIdentity(w, u, http.StatusBadRequest, rnd, err.Error())
			return
		}
		s.renderSetupIdentity(w, u, http.StatusOK, id, "")
	case "accept":
		source := r.PostFormValue("source")
		if source != cell.IdentityRandom {
			source = cell.IdentityAllocated
		}
		id, err := identityForm(r, source)
		if err == nil && r.PostFormValue("understood") != "on" {
			err = errors.New("Tick the box to confirm you read the notes.") //nolint:staticcheck // ST1005: shown on the page as a sentence
		}
		if err != nil {
			s.renderSetupIdentity(w, u, http.StatusBadRequest, id, err.Error())
			return
		}
		if err := s.Cells.SetSystemIdentity(r.Context(), u, id); err != nil {
			s.internalError(w, r, err)
			return
		}
		if err := s.Cells.SetIdentityApply(r.Context(), u, true); err != nil {
			s.internalError(w, r, err)
			return
		}
		http.Redirect(w, r, "/network-identity?done=The+network+identity+is+set.+Base+stations+take+it+when+they+connect.", http.StatusSeeOther)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}
