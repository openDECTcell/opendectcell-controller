// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

func (s *Server) stationRoutes() {
	if s.Cells == nil {
		return
	}
	s.mux.Handle("GET /stations", s.requireUser(s.stationList))
	s.mux.Handle("GET /stations/{id}", s.requireUser(s.stationPage))
	s.mux.Handle("POST /stations/{id}", s.requireUser(s.stationSave))
}

func (s *Server) stationList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	cells, err := s.Cells.ListOwned(r.Context(), u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	rows := make([]cellRow, len(cells))
	for i, c := range cells {
		rows[i] = cellRow{c, s.liveStatus(c.ID)}
	}
	s.render(w, http.StatusOK, "stations.html", page{Title: "My base stations", User: u, Data: rows})
}

type stationData struct {
	Cell     *cell.Cell
	Live     cell.Status
	Stats    stats.Station
	HasStats bool
	Lat, Lon string
	Models   []string
}

var boxModels = []string{"FRITZ!Box 7520", "FRITZ!Box 7520 B", "FRITZ!Box 7412"}

func (s *Server) stationView(r *http.Request, c *cell.Cell) stationData {
	d := stationData{Cell: c, Live: s.liveStatus(c.ID), Models: boxModels}
	if c.Latitude != nil && c.Longitude != nil {
		d.Lat = strconv.FormatFloat(*c.Latitude, 'f', -1, 64)
		d.Lon = strconv.FormatFloat(*c.Longitude, 'f', -1, 64)
	}
	if s.Stats != nil {
		if st, err := s.Stats.Station(r.Context(), c.ID); err == nil {
			d.Stats, d.HasStats = st, true
		}
	}
	return d
}

func (s *Server) stationByPath(w http.ResponseWriter, r *http.Request, u *auth.User) (*cell.Cell, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, u)
		return nil, false
	}
	c, err := s.Cells.GetOwned(r.Context(), u, id)
	if errors.Is(err, cell.ErrNotFound) {
		s.notFound(w, r, u)
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	return c, true
}

func (s *Server) stationPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	c, ok := s.stationByPath(w, r, u)
	if !ok {
		return
	}
	s.render(w, http.StatusOK, "station.html", page{Title: c.Name, User: u, Data: s.stationView(r, c), Notice: r.FormValue("done"), Leaflet: true})
}

// parseCoord reads a latitude or longitude in decimal degrees. An empty value
// is no coordinate: nil with a nil error, the cell then has no position.
func parseCoord(v string) (*float64, error) {
	v = strings.TrimSpace(strings.ReplaceAll(v, ",", "."))
	if v == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, errors.New("coordinates are decimal degrees, like 52.3759")
	}
	return &f, nil
}

func (s *Server) stationSave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	c, ok := s.stationByPath(w, r, u)
	if !ok {
		return
	}
	up := cell.ProfileUpdate{Name: r.PostFormValue("name"), Location: strings.TrimSpace(r.PostFormValue("location"))}
	up.BoxModel = r.PostFormValue("box_model")
	up.PositionPrecision = r.PostFormValue("precision")
	up.ShareName = r.PostFormValue("share_name") == "on"
	up.SharePosition = r.PostFormValue("share_position") == "on"
	up.ShareSpecs = r.PostFormValue("share_specs") == "on"
	up.ShareStats = r.PostFormValue("share_stats") == "on"
	lat, e1 := parseCoord(r.PostFormValue("lat"))
	lon, e2 := parseCoord(r.PostFormValue("lon"))
	err := errors.Join(e1, e2)
	if err == nil {
		up.Latitude, up.Longitude = lat, lon
		_, err = s.Cells.UpdateProfile(r.Context(), u, c.ID, up)
	}
	if err != nil {
		shown := *c
		shown.Name, shown.Location = up.Name, up.Location
		shown.Profile = up.Profile
		d := s.stationView(r, &shown)
		d.Lat, d.Lon = r.PostFormValue("lat"), r.PostFormValue("lon")
		s.render(w, http.StatusBadRequest, "station.html", page{Title: c.Name, User: u, Data: d, Error: strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + ".", Leaflet: true})
		return
	}
	http.Redirect(w, r, "/stations/"+c.ID.String()+"?done=Saved.", http.StatusSeeOther) //nolint:gosec // G710: our UUID
}
