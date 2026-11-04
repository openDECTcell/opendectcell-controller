// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

// navItem is one entry of the sidebar. Entries the controller has no page
// for yet still appear (Stub), so the structure is visible from the start
// and each stub page says what is planned.
type navItem struct {
	Label  string
	Href   string
	Icon   string
	Active bool
	Stub   bool
}

type navGroup struct {
	Title string
	Items []navItem
}

// navEntry places an item in the menu.
type navEntry struct {
	group string
	navItem
	admin    bool // admins only
	needCell bool // users who may enroll cells
}

// menu is the whole menu, in order. Groups are shown when they have items.
var menu = []navEntry{
	{group: "Phones", navItem: navItem{Label: "Overview", Href: "/", Icon: "layout-dashboard"}},
	{group: "Phones", navItem: navItem{Label: "Handsets", Href: "/handsets", Icon: "device-mobile"}},
	{group: "Phones", navItem: navItem{Label: "VoIP lines", Href: "/sip-accounts", Icon: "phone"}},
	{group: "Phones", navItem: navItem{Label: "Calls", Href: "/calls", Icon: "phone-call"}},
	{group: "Phones", navItem: navItem{Label: "Contacts", Href: "/contacts", Icon: "address-book"}},
	{group: "Base stations", navItem: navItem{Label: "Base stations", Href: "/stations", Icon: "antenna-bars-5"}},
	{group: "Base stations", navItem: navItem{Label: "Enroll a base station", Href: "/enroll", Icon: "plus"}, needCell: true},
	{group: "Base stations", navItem: navItem{Label: "Organizations and trust", Href: "/organizations", Icon: "users"}},
	{group: "Base stations", navItem: navItem{Label: "Public map", Href: "/map", Icon: "map-2"}},
	{group: "Insight", navItem: navItem{Label: "Statistics", Href: "/statistics", Icon: "chart-bar"}},
	{group: "Insight", navItem: navItem{Label: "Logging", Href: "/logging", Icon: "list-details"}},
	{group: "Administration", navItem: navItem{Label: "Dashboard", Href: "/admin", Icon: "activity-heartbeat"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "All base stations", Href: "/cells", Icon: "building-broadcast-tower"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "Network identity", Href: "/network-identity", Icon: "id"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "Users", Href: "/admin/users", Icon: "users"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "Release a handset", Href: "/admin/handset-release", Icon: "device-mobile"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "All calls", Href: "/admin/calls", Icon: "phone-call"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "Statistics", Href: "/admin/statistics", Icon: "report-analytics"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "Logging", Href: "/admin/logging", Icon: "bug"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "Audit log", Href: "/admin/audit", Icon: "history"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "Settings", Href: "/admin/settings", Icon: "settings"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "System", Href: "/admin/system", Icon: "server"}, admin: true},
	{group: "Administration", navItem: navItem{Label: "Backup", Href: "/admin/backup", Icon: "database"}, admin: true},
	{group: "Account", navItem: navItem{Label: "Account", Href: "/account", Icon: "user"}},
}

// navFor builds the menu for u with the entry of path marked: the longest
// href that is path or a parent of it.
func navFor(u *auth.User, path string) []navGroup {
	best, bestLen := "", 0
	for _, e := range menu {
		if hrefMatches(e.Href, path) && len(e.Href) > bestLen && visible(e, u) {
			best, bestLen = e.Href, len(e.Href)
		}
	}
	var out []navGroup
	for _, e := range menu {
		if !visible(e, u) {
			continue
		}
		it := e.navItem
		it.Active = it.Href == best
		if n := len(out); n > 0 && out[n-1].Title == e.group {
			out[n-1].Items = append(out[n-1].Items, it)
		} else {
			out = append(out, navGroup{Title: e.group, Items: []navItem{it}})
		}
	}
	return out
}

func visible(e navEntry, u *auth.User) bool {
	switch {
	case u == nil:
		return false
	case e.admin:
		return u.IsAdmin()
	case e.needCell:
		return u.CanAddCells()
	}
	return true
}

func hrefMatches(href, path string) bool {
	if href == "/" {
		return path == "/"
	}
	return path == href || strings.HasPrefix(path, href+"/")
}

// section is the group title of the current page, for the page header.
func section(groups []navGroup) string {
	for _, g := range groups {
		for _, it := range g.Items {
			if it.Active {
				return g.Title
			}
		}
	}
	return ""
}

// brand is what the layout shows of the instance's identity.
type brand struct {
	Name    string
	Tagline string
	HasLogo bool
	Rev     int64 // the logo's version, so a changed logo is not served from cache
}

type brandCache struct {
	mu     sync.Mutex
	at     time.Time
	val    brand
	have   bool
	tiles  settings.Map
	origin string // the tile server's origin, for the Content-Security-Policy
}

// mapConfig returns the tile settings and their origin ("" for no tiles).
func (s *Server) mapConfig(ctx context.Context) (settings.Map, string) {
	s.brand(ctx)
	s.brands.mu.Lock()
	defer s.brands.mu.Unlock()
	return s.brands.tiles, s.brands.origin
}

func (s *Server) brand(ctx context.Context) brand {
	if s.Settings == nil {
		return brand{Name: settings.DefaultName}
	}
	s.brands.mu.Lock()
	defer s.brands.mu.Unlock()
	if s.brands.have && time.Since(s.brands.at) < 5*time.Second {
		return s.brands.val
	}
	in, err := s.Settings.Instance(ctx)
	if err != nil {
		s.Log.Error("instance settings", "err", err)
		if s.brands.have {
			return s.brands.val
		}
		return brand{Name: settings.DefaultName}
	}
	b := brand{Name: in.Name, Tagline: in.Tagline, HasLogo: in.HasLogo}
	if in.HasLogo {
		if _, _, up, ok, err := s.Settings.Logo(ctx); err == nil && ok {
			b.Rev = up.Unix()
		}
	}
	m, err := s.Settings.Map(ctx)
	if err != nil {
		s.Log.Error("map settings", "err", err)
	}
	origin, verr := m.Validate()
	if verr != nil { // a stored value that no longer validates: no tiles
		m, origin = settings.Map{}, ""
	}
	s.brands.val, s.brands.at, s.brands.have = b, time.Now(), true
	s.brands.tiles, s.brands.origin = m, origin
	return b
}

// brandChanged drops the cached brand, after an admin saved a new one.
func (s *Server) brandChanged() {
	s.brands.mu.Lock()
	s.brands.have = false
	s.brands.mu.Unlock()
}

// navWriter carries the request to render, which only gets the writer: the
// layout needs the path for the active menu entry and the context for the
// instance's name.
type navWriter struct {
	http.ResponseWriter
	r *http.Request
}

func (w *navWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush passes through to the wrapped writer when it can flush, so streaming
// responses still work behind the wrapper.
func (w *navWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func withRequest(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&navWriter{ResponseWriter: w, r: r}, r)
	})
}

// icons is the embedded set of Tabler icons (MIT), inlined by the layout.
func (s *Server) iconFunc() func(name string) template.HTML {
	return func(name string) template.HTML {
		b, err := staticFS.ReadFile("static/vendor/tabler/icons/" + name + ".svg")
		if err != nil {
			return ""
		}
		// the files are our own, vendored, trusted markup
		return template.HTML(strings.ReplaceAll(string(b), "\n", " ")) //nolint:gosec // G203: embedded file
	}
}
