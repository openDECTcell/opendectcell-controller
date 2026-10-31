// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

func (s *Server) settingsRoutes() {
	if s.Settings == nil {
		return
	}
	s.mux.Handle("GET /admin/settings", s.requireAdmin(s.settingsPage))
	s.mux.Handle("POST /admin/settings/general", s.requireAdmin(s.settingsGeneral))
	s.mux.Handle("POST /admin/settings/registration", s.requireAdmin(s.settingsRegistration))
	s.mux.Handle("POST /admin/settings/map", s.requireAdmin(s.settingsMap))
}

type settingsData struct {
	Instance     settings.Instance
	Registration settings.Registration
	Domains      string
	LocalLogin   bool
	OIDC         bool
	OIDCName     string
	OIDCIssuer   string
	BaseURL      string
	Map          settings.Map
	MapChoice    string // none, osm or custom
	SMTP         bool
	SMTPHost     string
}

func (s *Server) settingsView(r *http.Request) (settingsData, error) {
	var d settingsData
	var err error
	if s.Settings == nil {
		return d, errors.New("settings are not available")
	}
	if d.Instance, err = s.Settings.Instance(r.Context()); err != nil {
		return d, err
	}
	if d.Registration, err = s.Settings.Registration(r.Context()); err != nil {
		return d, err
	}
	d.Domains = strings.Join(d.Registration.AllowedDomains, ", ")
	d.LocalLogin, d.OIDC, d.OIDCName = s.LocalLogin, s.OIDC != nil, s.OIDCName
	if s.OIDC != nil {
		d.OIDCIssuer = s.OIDC.Config.Issuer
	}
	if s.BaseURL != nil {
		d.BaseURL = s.BaseURL.String()
	}
	if d.Map, err = s.Settings.Map(r.Context()); err != nil {
		return d, err
	}
	switch d.Map.TileURL {
	case "":
		d.MapChoice = "none"
	case settings.OSMTileURL:
		d.MapChoice = "osm"
	default:
		d.MapChoice = "custom"
	}
	d.SMTP, d.SMTPHost = s.mailOn(), s.SMTPHost
	return d, nil
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, u *auth.User, status int, p page) {
	d, err := s.settingsView(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	p.Title, p.User, p.Data = "Settings", u, d
	s.render(w, status, "admin_settings.html", p)
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.renderSettings(w, r, u, http.StatusOK, page{Notice: r.FormValue("done")})
}

func (s *Server) settingsGeneral(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, settings.MaxLogoBytes+(64<<10))
	if err := r.ParseMultipartForm(settings.MaxLogoBytes + (64 << 10)); err != nil { //nolint:gosec // G120: the body is capped by the MaxBytesReader above
		s.renderSettings(w, r, u, http.StatusBadRequest, page{Error: "The form was too large or malformed. A logo may be at most 256 KiB."})
		return
	}
	name, tag := strings.TrimSpace(r.PostFormValue("name")), strings.TrimSpace(r.PostFormValue("tagline"))
	if name == "" || len(name) > 64 || len(tag) > 120 {
		s.renderSettings(w, r, u, http.StatusBadRequest, page{Error: "The name needs 1 to 64 characters, the tagline at most 120."})
		return
	}
	cur, err := s.Settings.Instance(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var logo []byte
	if f, _, err := r.FormFile("logo"); err == nil {
		logo, err = io.ReadAll(io.LimitReader(f, settings.MaxLogoBytes+1))
		f.Close()
		if err == nil {
			_, err = settings.LogoType(logo)
		}
		if err != nil {
			s.renderSettings(w, r, u, http.StatusBadRequest, page{Error: settings.ErrBadLogo.Error() + "."})
			return
		}
	}
	changed := []string{}
	if name != cur.Name || tag != cur.Tagline {
		cur.Name, cur.Tagline = name, tag
		if err := s.Settings.Put(r.Context(), settings.KeyInstance, cur); err != nil {
			s.internalError(w, r, err)
			return
		}
		changed = append(changed, "name")
	}
	switch {
	case logo != nil:
		err = s.Settings.SetLogo(r.Context(), logo)
		changed = append(changed, "logo")
	case r.PostFormValue("remove_logo") == "on" && cur.HasLogo:
		err = s.Settings.ClearLogo(r.Context())
		changed = append(changed, "logo removed")
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(changed) > 0 {
		_ = audit.Record(r.Context(), s.Pool, audit.Entry{Action: "settings.update", ObjectType: "setting", ObjectID: settings.KeyInstance, Details: map[string]any{"changed": changed}})
	}
	s.brandChanged()
	http.Redirect(w, r, "/admin/settings?done=Saved.", http.StatusSeeOther)
}

func (s *Server) settingsRegistration(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var doms []string
	for _, d := range strings.FieldsFunc(r.PostFormValue("domains"), func(c rune) bool { return c == ',' || c == ' ' || c == '\n' || c == ';' }) {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
		if d == "" {
			continue
		}
		if strings.ContainsAny(d, "/@ ") || !strings.Contains(d, ".") || len(d) > 253 {
			s.renderSettings(w, r, u, http.StatusBadRequest, page{Error: "\"" + d + "\" is not a domain name."})
			return
		}
		doms = append(doms, d)
	}
	reg := settings.Registration{Enabled: r.PostFormValue("enabled") == "on", RequireApproval: r.PostFormValue("require_approval") == "on", AllowedDomains: doms}
	if err := s.Settings.Put(r.Context(), settings.KeyRegistration, reg); err != nil {
		s.internalError(w, r, err)
		return
	}
	_ = audit.Record(r.Context(), s.Pool, audit.Entry{Action: "settings.update", ObjectType: "setting", ObjectID: settings.KeyRegistration,
		Details: map[string]any{"enabled": reg.Enabled, "require_approval": reg.RequireApproval, "domains": doms}})
	http.Redirect(w, r, "/admin/settings?done=Saved.", http.StatusSeeOther)
}

func (s *Server) settingsMap(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var m settings.Map
	public := r.PostFormValue("public") == "on"
	switch r.PostFormValue("choice") {
	case "none":
	case "osm":
		m = settings.Map{TileURL: settings.OSMTileURL, Attribution: settings.OSMAttribution}
	case "custom":
		m = settings.Map{TileURL: strings.TrimSpace(r.PostFormValue("tile_url")), Attribution: strings.TrimSpace(r.PostFormValue("attribution"))}
	default:
		s.renderSettings(w, r, u, http.StatusBadRequest, page{Error: "Choose one of the three map options."})
		return
	}
	m.Public = public
	if _, err := m.Validate(); err != nil {
		s.renderSettings(w, r, u, http.StatusBadRequest, page{Error: err.Error() + "."})
		return
	}
	if err := s.Settings.Put(r.Context(), settings.KeyMap, m); err != nil {
		s.internalError(w, r, err)
		return
	}
	_ = audit.Record(r.Context(), s.Pool, audit.Entry{Action: "settings.update", ObjectType: "setting", ObjectID: settings.KeyMap,
		Details: map[string]any{"tile_url": m.TileURL, "public": m.Public}})
	s.brandChanged()
	http.Redirect(w, r, "/admin/settings?done=Saved.", http.StatusSeeOther)
}
