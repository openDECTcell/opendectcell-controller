// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
)

func (s *Server) userRoutes() {
	s.mux.Handle("GET /admin/users/new", s.requireAdmin(s.userNew))
	s.mux.Handle("POST /admin/users", s.requireAdmin(s.userCreate))
	s.mux.Handle("GET /admin/users/{id}", s.requireAdmin(s.userEdit))
	s.mux.Handle("POST /admin/users/{id}", s.requireAdmin(s.userUpdate))
	s.mux.Handle("POST /admin/users/{id}/password", s.requireAdmin(s.userPassword))
	s.mux.Handle("POST /admin/users/{id}/approve", s.requireAdmin(s.userApprove))
	s.mux.Handle("POST /admin/users/{id}/delete", s.requireAdmin(s.userDelete))
}

type userForm struct {
	Target *auth.User
	New    bool
	Self   bool
	// Handsets and Cells are the ones the user disclosed to the administrators.
	Handsets []*handset.Handset
	Cells    []*cell.Cell
}

func (s *Server) renderUser(w http.ResponseWriter, r *http.Request, status int, u *auth.User, f userForm, p page) {
	p.User, p.Data = u, f
	if !f.New && f.Target != nil && f.Target.ID != uuid.Nil {
		// what this user chose to disclose to the administrators, and nothing else of theirs
		if s.Handsets != nil {
			f.Handsets, _ = s.Handsets.Disclosed(r.Context(), u, f.Target.ID)
		}
		if s.Cells != nil {
			f.Cells, _ = s.Cells.ListDisclosed(r.Context(), u, f.Target.ID)
		}
		p.Data = f
	}
	p.Title = "New user"
	if !f.New {
		p.Title = f.Target.Username
	}
	s.render(w, status, "admin_user.html", p)
}

func (s *Server) userNew(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.renderUser(w, r, http.StatusOK, u, userForm{Target: &auth.User{Role: auth.RoleUser}, New: true}, page{})
}

func (s *Server) userCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	n := auth.NewLocalUser{Username: r.PostFormValue("username"), DisplayName: r.PostFormValue("display_name"),
		Email: r.PostFormValue("email"), Password: r.PostFormValue("password"), Role: auth.Role(r.PostFormValue("role")),
		MayAddCells: r.PostFormValue("may_add_cells") == "on"}
	nu, err := s.Auth.CreateLocalUser(r.Context(), n)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, auth.ErrUsernameTaken) {
			msg = "That username exists already."
		}
		shown := &auth.User{Username: n.Username, DisplayName: n.DisplayName, Email: n.Email, Role: n.Role, MayAddCells: n.MayAddCells}
		s.renderUser(w, r, http.StatusBadRequest, u, userForm{Target: shown, New: true}, page{Error: msg})
		return
	}
	http.Redirect(w, r, "/admin/users/"+nu.ID.String()+"?done=Created.", http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

func (s *Server) userByPath(w http.ResponseWriter, r *http.Request, u *auth.User) (*auth.User, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, u)
		return nil, false
	}
	t, err := s.Auth.UserByID(r.Context(), s.Pool, id)
	if errors.Is(err, auth.ErrNotFound) {
		s.notFound(w, r, u)
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	return t, true
}

func (s *Server) userEdit(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if t, ok := s.userByPath(w, r, u); ok {
		s.renderUser(w, r, http.StatusOK, u, userForm{Target: t, Self: t.ID == u.ID}, page{Notice: r.FormValue("done")})
	}
}

func (s *Server) userUpdate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	t, ok := s.userByPath(w, r, u)
	if !ok {
		return
	}
	dn, em := r.PostFormValue("display_name"), r.PostFormValue("email")
	role := auth.Role(r.PostFormValue("role"))
	may, dis := r.PostFormValue("may_add_cells") == "on", r.PostFormValue("disabled") == "on"
	_, err := s.Auth.UpdateUser(r.Context(), u, t.ID, auth.UserUpdate{DisplayName: &dn, Email: &em, Role: &role, MayAddCells: &may, Disabled: &dis})
	if err == nil && dis && !t.Disabled {
		s.disableCellsOf(r.Context(), t.ID)
	}
	if err != nil {
		msg := "Could not save: " + err.Error()
		if errors.Is(err, auth.ErrSelf) {
			msg = "You cannot remove your own admin role or disable yourself."
		}
		s.renderUser(w, r, http.StatusBadRequest, u, userForm{Target: t, Self: t.ID == u.ID}, page{Error: msg})
		return
	}
	http.Redirect(w, r, "/admin/users/"+t.ID.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

// disableCellsOf switches off the base stations of a user who was just disabled, and tells their links.
func (s *Server) disableCellsOf(ctx context.Context, user uuid.UUID) {
	if s.Cells == nil {
		return
	}
	ids, err := s.Cells.DisableCellsOf(ctx, user)
	if err != nil {
		s.Log.Error("base stations of a disabled user not switched off", "user", user, "err", err)
		return
	}
	if s.CellLink != nil {
		for _, id := range ids {
			s.CellLink.NotifyState(id, wire.StateDisabled, "its operator was disabled")
		}
	}
}

func (s *Server) userPassword(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	t, ok := s.userByPath(w, r, u)
	if !ok {
		return
	}
	if t.AuthSource != "local" {
		s.renderUser(w, r, http.StatusBadRequest, u, userForm{Target: t}, page{Error: "This user signs in with single sign-on."})
		return
	}
	if err := s.Auth.SetPassword(r.Context(), t.ID, r.PostFormValue("password"), nil); err != nil {
		s.renderUser(w, r, http.StatusBadRequest, u, userForm{Target: t, Self: t.ID == u.ID}, page{Error: err.Error()})
		return
	}
	http.Redirect(w, r, "/admin/users/"+t.ID.String()+"?done=Password+set;+their+sessions+were+ended.", http.StatusSeeOther) //nolint:gosec // G710: our UUID
}

func (s *Server) userDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	t, ok := s.userByPath(w, r, u)
	if !ok {
		return
	}
	err := s.Auth.DeleteUser(r.Context(), u, t.ID)
	switch {
	case errors.Is(err, auth.ErrSelf):
		s.renderUser(w, r, http.StatusBadRequest, u, userForm{Target: t, Self: true}, page{Error: "You cannot delete yourself."})
	case errors.Is(err, auth.ErrHasObjects):
		s.renderUser(w, r, http.StatusConflict, u, userForm{Target: t}, page{Error: "This user still owns handsets or VoIP lines. Reassign or delete them first."})
	case err != nil:
		s.internalError(w, r, err)
	default:
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
	}
}
