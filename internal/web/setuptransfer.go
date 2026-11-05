// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/transfer"
)

// importMaxSetupBytes is the largest setup file an import reads.
const importMaxSetupBytes = 5 << 20

// kdfSlots bounds how many exports and imports derive a key at once: each takes 64 MiB for a moment.
var kdfSlots = make(chan struct{}, 2)

func (s *Server) setupRoutes() {
	s.mux.Handle("GET /account/setup", s.requireUser(s.setupPage))
	s.mux.Handle("POST /account/setup/export", s.requireUser(s.setupExport))
	s.mux.Handle("POST /account/setup/import", s.requireUser(s.setupImport))
	s.mux.Handle("GET /account/setup/lines-template.csv", s.requireUser(s.setupLinesTemplate))
	s.mux.Handle("POST /account/setup/lines-import", s.requireUser(s.setupLinesImport))
}

type setupData struct {
	Local  bool
	Report *transfer.Report
	// Asked says which part of the export form was ticked when it came back with an error.
	Min int
}

func (s *Server) renderSetup(w http.ResponseWriter, u *auth.User, status int, d setupData, p page) {
	d.Local, d.Min = u.AuthSource == "local", transfer.MinPassphrase
	p.Title, p.User, p.Data = "Move or back up my setup", u, d
	s.render(w, status, "account_setup.html", p)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if s.Transfer == nil {
		http.NotFound(w, r)
		return
	}
	s.renderSetup(w, u, http.StatusOK, setupData{}, page{})
}

// confirmUser asks the user to prove it is them (the password; for single sign-on, their user name): an export with keys must not
// leave on a session someone walked away from. "" means confirmed.
func (s *Server) confirmUser(r *http.Request, u *auth.User) string {
	if u.AuthSource == "local" {
		if err := s.Auth.CheckPassword(r.Context(), u.ID, r.PostFormValue("password"), addrFrom(r.Context()).String()); err != nil {
			if errors.Is(err, auth.ErrRateLimited) {
				return "Too many failed attempts. Try again in a few minutes."
			}
			return "The password is wrong."
		}
		return ""
	}
	if !strings.EqualFold(strings.TrimSpace(r.PostFormValue("username")), u.Username) {
		return "Type your user name to confirm."
	}
	return ""
}

func (s *Server) setupExport(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if s.Transfer == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	fail := func(status int, msg string) { s.renderSetup(w, u, status, setupData{}, page{Error: msg}) }
	o := transfer.Options{Handsets: r.PostFormValue("handsets") == "on", Lines: r.PostFormValue("lines") == "on",
		Contacts: r.PostFormValue("contacts") == "on", Secrets: r.PostFormValue("secrets") == "on"}
	if !o.Handsets && !o.Lines && !o.Contacts {
		fail(http.StatusBadRequest, "Choose what to export.")
		return
	}
	// keys and passwords only make sense for what carries them
	o.Secrets = o.Secrets && (o.Handsets || o.Lines)
	pass := r.PostFormValue("passphrase")
	if o.Secrets {
		if len([]rune(pass)) < transfer.MinPassphrase {
			fail(http.StatusBadRequest, "The passphrase must have at least 12 characters.")
			return
		}
		if pass != r.PostFormValue("passphrase2") {
			fail(http.StatusBadRequest, "The two passphrases differ.")
			return
		}
		if msg := s.confirmUser(r, u); msg != "" {
			fail(http.StatusBadRequest, msg)
			return
		}
	}
	set, err := s.Transfer.Export(r.Context(), u, o)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var file []byte
	if o.Secrets {
		select {
		case kdfSlots <- struct{}{}:
			defer func() { <-kdfSlots }()
		default:
			fail(http.StatusServiceUnavailable, "The controller is busy encrypting other files. Try again in a moment.")
			return
		}
		file, err = transfer.Seal(set, pass)
	} else {
		file, err = transfer.Plain(set)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	_ = audit.Record(r.Context(), s.Pool, audit.Entry{Action: "setup.export", ObjectType: "user", ObjectID: u.ID.String(),
		Details: map[string]any{"handsets": len(set.Handsets), "lines": len(set.Lines), "contacts": set.Contacts != "", "encrypted": o.Secrets}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="opendectcell-setup-`+time.Now().Format("2006-01-02")+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(file) //nolint:gosec // G705: a JSON file sent as a download (attachment), not a page
}

func (s *Server) setupImport(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if s.Transfer == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, importMaxSetupBytes+(64<<10))
	fail := func(status int, msg string) { s.renderSetup(w, u, status, setupData{}, page{Error: msg}) }
	if err := r.ParseMultipartForm(importMaxSetupBytes); err != nil { //nolint:gosec // G120: the body is capped by the MaxBytesReader above
		fail(http.StatusRequestEntityTooLarge, "The file could not be read, or it is larger than 5 MB.")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		fail(http.StatusBadRequest, "Choose the setup file.")
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, importMaxSetupBytes+1))
	if err != nil || len(raw) > importMaxSetupBytes {
		fail(http.StatusRequestEntityTooLarge, "The file is larger than 5 MB.")
		return
	}
	select {
	case kdfSlots <- struct{}{}:
		defer func() { <-kdfSlots }()
	default:
		fail(http.StatusServiceUnavailable, "The controller is busy with other files. Try again in a moment.")
		return
	}
	set, _, err := transfer.Open(raw, r.PostFormValue("passphrase"))
	if err != nil {
		fail(http.StatusBadRequest, setupErrorText(err))
		return
	}
	dry := r.PostFormValue("mode") != "import"
	if !dry && set.HasSecrets() {
		// keys and passwords in the account of whoever imports them: the same proof as for an export
		if msg := s.confirmUser(r, u); msg != "" {
			fail(http.StatusBadRequest, msg)
			return
		}
	}
	rep, err := s.Transfer.Import(r.Context(), u, set, dry)
	if err != nil {
		s.Log.Warn("setup import", "err", err)
		fail(http.StatusBadRequest, "The file could not be imported.")
		return
	}
	if s.HandsetChanged != nil {
		for _, id := range rep.ChangedHandsets {
			s.HandsetChanged(id)
		}
	}
	s.renderSetup(w, u, http.StatusOK, setupData{Report: &rep}, page{})
}

// setupErrorText is what a person reads of an error from opening a file. The messages carry no value from the file.
func setupErrorText(err error) string {
	switch {
	case errors.Is(err, transfer.ErrPassphrase), errors.Is(err, transfer.ErrNeedPassphrase), errors.Is(err, transfer.ErrFormat), errors.Is(err, transfer.ErrPlainSecrets):
		return strings.TrimPrefix(err.Error(), "transfer: ")
	}
	return "The file could not be read."
}

func (s *Server) setupLinesTemplate(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	if s.Transfer == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="voip-lines-template.csv"`)
	_, _ = io.WriteString(w, transfer.LinesCSVTemplate)
}

// setupLinesImport reads VoIP lines from a spreadsheet: the same import as a setup file, with the same check first. The passwords in
// such a file are the user's own, in a file the user made; they are not kept or shown.
func (s *Server) setupLinesImport(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if s.Transfer == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, importMaxBytes+(64<<10))
	fail := func(status int, msg string) { s.renderSetup(w, u, status, setupData{}, page{Error: msg}) }
	if err := r.ParseMultipartForm(importMaxBytes); err != nil { //nolint:gosec // G120: the body is capped by the MaxBytesReader above
		fail(http.StatusRequestEntityTooLarge, "The file could not be read, or it is larger than 2 MB.")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		fail(http.StatusBadRequest, "Choose a .csv file.")
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, importMaxBytes+1))
	if err != nil || len(raw) > importMaxBytes {
		fail(http.StatusRequestEntityTooLarge, "The file is larger than 2 MB.")
		return
	}
	parsed := transfer.ParseLinesCSV(bytes.NewReader(raw))
	if len(parsed.Lines) == 0 {
		fail(http.StatusBadRequest, "No line could be read. "+strings.Join(parsed.Skipped, " "))
		return
	}
	dry := r.PostFormValue("mode") != "import"
	rep, err := s.Transfer.Import(r.Context(), u, transfer.Setup{Lines: parsed.Lines}, dry)
	if err != nil {
		s.Log.Warn("lines import", "err", err)
		fail(http.StatusBadRequest, "The file could not be imported.")
		return
	}
	for _, sk := range parsed.Skipped {
		rep.Items = append(rep.Items, transfer.Item{Kind: "row", Name: "", Action: "fail", Note: sk})
		rep.Failed++
	}
	s.renderSetup(w, u, http.StatusOK, setupData{Report: &rep}, page{})
}
