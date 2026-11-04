// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
)

const importMaxBytes = 2 << 20

func (s *Server) contactExchangeRoutes() {
	s.mux.Handle("GET /contacts/export.vcf", s.requireUser(s.contactsExportVCard))
	s.mux.Handle("GET /contacts/export.csv", s.requireUser(s.contactsExportCSV))
	s.mux.Handle("POST /contacts/import", s.requireUser(s.contactsImport))
}

// importResult is what an import did, for the page that follows it.
type importResult struct {
	Added      int
	Duplicates int
	Failed     int
	Skipped    []string // what the file held that could not be used, in words (at most 12 are shown)
	More       int
}

// own are the user's personal contacts: what is exported, and what an import is compared with.
func (s *Server) ownContacts(r *http.Request, u *auth.User) ([]*contacts.Contact, error) {
	all, err := s.Contacts.List(r.Context(), u, "", "")
	if err != nil {
		return nil, err
	}
	var mine []*contacts.Contact
	for _, c := range all {
		if c.OwnerID != nil && *c.OwnerID == u.ID {
			mine = append(mine, c)
		}
	}
	return mine, nil
}

func (s *Server) contactsExportVCard(w http.ResponseWriter, r *http.Request, u *auth.User) {
	mine, err := s.ownContacts(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="contacts-`+time.Now().Format("2006-01-02")+`.vcf"`)
	w.Header().Set("Cache-Control", "no-store")
	if err := contacts.WriteVCards(w, mine); err != nil {
		s.Log.Info("contacts export: the client went away", "err", err)
	}
}

func (s *Server) contactsExportCSV(w http.ResponseWriter, r *http.Request, u *auth.User) {
	mine, err := s.ownContacts(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="contacts-`+time.Now().Format("2006-01-02")+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("\xEF\xBB\xBF"))
	if err := contacts.WriteCSV(w, mine, csvSafe); err != nil {
		s.Log.Info("contacts export: the client went away", "err", err)
	}
}

// sameContact: a contact the user already has: the same name and a number in common.
func sameContact(a, b contacts.Contact) bool {
	if !strings.EqualFold(a.LastName, b.LastName) || !strings.EqualFold(a.FirstName, b.FirstName) {
		return false
	}
	for _, x := range a.Numbers {
		for _, y := range b.Numbers {
			if x.Number == y.Number {
				return true
			}
		}
	}
	return false
}

func (s *Server) contactsImport(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, importMaxBytes+(64<<10))
	if err := r.ParseMultipartForm(importMaxBytes); err != nil { //nolint:gosec // G120: the body is capped by the MaxBytesReader above
		s.renderContacts(w, r, u, http.StatusRequestEntityTooLarge, contactsData{}, page{Error: "The file could not be read, or it is larger than 2 MB."})
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		s.renderContacts(w, r, u, http.StatusBadRequest, contactsData{}, page{Error: "Choose a .vcf or .csv file."})
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, importMaxBytes+1))
	if err != nil || len(raw) > importMaxBytes {
		s.renderContacts(w, r, u, http.StatusRequestEntityTooLarge, contactsData{}, page{Error: "The file is larger than 2 MB."})
		return
	}
	var im contacts.Imported
	if t := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(string(raw[:min(len(raw), 64)]), "\uFEFF"))); strings.HasPrefix(t, "BEGIN:VCARD") {
		im = contacts.ParseVCards(bytes.NewReader(raw))
	} else {
		im = contacts.ParseCSV(bytes.NewReader(raw))
	}
	have, err := s.ownContacts(r, u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var res importResult
	for _, c := range im.Contacts {
		dup := false
		for _, h := range have {
			if sameContact(c, *h) {
				dup = true
				break
			}
		}
		if dup {
			res.Duplicates++
			continue
		}
		created, err := s.Contacts.Create(r.Context(), u, c, false)
		var v contacts.ValidationError
		switch {
		case errors.As(err, &v):
			res.Failed++
			res.Skipped = append(res.Skipped, strings.TrimSpace(c.FirstName+" "+c.LastName)+": "+v.Error())
		case err != nil:
			s.internalError(w, r, err)
			return
		default:
			res.Added++
			have = append(have, created)
		}
	}
	res.Skipped = append(im.Skipped, res.Skipped...)
	if len(res.Skipped) > 12 {
		res.More = len(res.Skipped) - 12
		res.Skipped = res.Skipped[:12]
	}
	s.renderContacts(w, r, u, http.StatusOK, contactsData{Import: &res}, page{})
}
