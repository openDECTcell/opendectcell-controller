// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/legal"
)

func (s *Server) legalRoutes() {
	if s.Legal == nil {
		return
	}
	s.mux.HandleFunc("GET /legal", s.legalPage)
	s.mux.Handle("GET /legal/accept", s.requireUserAny(s.legalAcceptPage))
	s.mux.Handle("POST /legal/accept", s.requireUserAny(s.legalAcceptPost))
	s.mux.Handle("GET /admin/legal", s.requireAdmin(s.adminLegalPage))
	s.mux.Handle("POST /admin/legal", s.requireAdmin(s.adminLegalPost))
}

// withLegal marks a user who has legal texts to sign off. A failure to find out marks them too: no access is the safe side.
func (s *Server) withLegal(ctx context.Context, u *auth.User) *auth.User {
	if s.Legal == nil || u == nil {
		return u
	}
	pending, err := s.Legal.Pending(ctx, u.ID)
	if err != nil {
		s.Log.Error("legal sign-off lookup", "err", err)
	}
	if err == nil && len(pending) == 0 {
		return u
	}
	c := *u
	c.LegalPending = true
	return &c
}

// legalShown is the list of enabled texts for the menu and the footer; a failure reads as none.
func (s *Server) legalShown(ctx context.Context) []legal.Shown {
	if s.Legal == nil {
		return nil
	}
	docs, err := s.Legal.Enabled(ctx)
	if err != nil {
		s.Log.Error("legal texts", "err", err)
		return nil
	}
	return docs
}

type legalPageData struct {
	Docs []legal.Shown
}

// legalPage shows every enabled text, each in a window of its own that scrolls. Anyone can read it, also without a login.
func (s *Server) legalPage(w http.ResponseWriter, r *http.Request) {
	docs := s.legalShown(r.Context())
	if len(docs) == 0 {
		s.notFound(w, r, auth.UserFrom(r.Context()))
		return
	}
	s.render(w, http.StatusOK, "legal.html", page{Title: "Legal notice", User: auth.UserFrom(r.Context()), Data: legalPageData{Docs: docs}})
}

type legalAcceptData struct {
	Username string
	Docs     []legal.Shown
}

func (s *Server) renderLegalAccept(w http.ResponseWriter, r *http.Request, u *auth.User, status int, msg string) {
	pending, err := s.Legal.Pending(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(pending) == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// shown like a sign-in page: nothing else of the controller is open to this user yet
	s.render(w, status, "legal_accept.html", page{Title: "Please read and sign off", Error: msg, Data: legalAcceptData{Username: u.Username, Docs: pending}})
}

func (s *Server) legalAcceptPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.renderLegalAccept(w, r, u, http.StatusOK, "")
}

// signedFrom reads the ticked boxes of a sign-off form: each box carries the version of the text the user was shown.
func signedFrom(r *http.Request) map[legal.Kind]int {
	signed := map[legal.Kind]int{}
	for _, k := range legal.Kinds {
		if v, err := strconv.Atoi(r.PostFormValue("accept_" + string(k))); err == nil && v > 0 {
			signed[k] = v
		}
	}
	return signed
}

func (s *Server) legalAcceptPost(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	err := s.Legal.Accept(r.Context(), u.ID, signedFrom(r))
	switch {
	case errors.Is(err, legal.ErrNotAll):
		s.renderLegalAccept(w, r, u, http.StatusBadRequest, "Please tick every box. Without it there is no access.")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type adminLegalData struct {
	Kinds []adminLegalKind
}

type adminLegalKind struct {
	Kind     legal.Kind
	Title    string
	Enabled  bool
	Text     string
	Version  int
	Standard string
	Needed   bool
}

func (s *Server) renderAdminLegal(w http.ResponseWriter, r *http.Request, u *auth.User, status int, p page, in map[legal.Kind]legal.Input) {
	c, err := s.Legal.Config(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var d adminLegalData
	for _, k := range legal.Kinds {
		doc := c[k]
		kk := adminLegalKind{Kind: k, Title: k.Title(), Enabled: doc.Enabled, Text: doc.Text, Version: doc.Version, Standard: k.Standard(), Needed: k.NeedsText()}
		if i, ok := in[k]; ok { // a form that was refused is shown again as it was sent
			kk.Enabled, kk.Text = i.Enabled, i.Text
		}
		d.Kinds = append(d.Kinds, kk)
	}
	p.Title, p.User, p.Data = "Legal texts", u, d
	s.render(w, status, "admin_legal.html", p)
}

func (s *Server) adminLegalPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.renderAdminLegal(w, r, u, http.StatusOK, page{Notice: r.FormValue("done")}, nil)
}

func (s *Server) adminLegalPost(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*(legal.MaxText+1024)+(8<<10))
	if err := r.ParseForm(); err != nil {
		s.renderAdminLegal(w, r, u, http.StatusBadRequest, page{Error: "The form was too large or malformed."}, nil)
		return
	}
	in := map[legal.Kind]legal.Input{}
	for _, k := range legal.Kinds {
		in[k] = legal.Input{Enabled: r.PostFormValue("enabled_"+string(k)) == "on", Text: r.PostFormValue("text_" + string(k))}
	}
	if err := s.Legal.Save(r.Context(), in); err != nil {
		if errors.Is(err, legal.ErrInvalid) {
			msg := strings.TrimPrefix(err.Error(), legal.ErrInvalid.Error()+": ")
			s.renderAdminLegal(w, r, u, http.StatusBadRequest, page{Error: strings.ToUpper(msg[:1]) + msg[1:] + "."}, in)
			return
		}
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/legal?done=Saved. Everyone, you included, signs off again where a text was switched on or changed.", http.StatusSeeOther)
}
