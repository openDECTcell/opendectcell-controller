// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/alerts"
	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/backup"
)

func (s *Server) backupRoutes() {
	s.mux.Handle("GET /admin/backup", s.requireAdmin(s.backupPage))
	s.mux.Handle("GET /admin/backup/{name}", s.requireAdmin(s.backupDownload))
}

type backupFile struct {
	Name string
	Size int64
	At   time.Time
}

type backupView struct {
	Configured bool // a backup directory is set
	Status     *backup.Status
	Problem    string
	Files      []backupFile
	Latest     string
	LastGood   time.Time
	Err        string // the directory or the state could not be read
}

func (s *Server) backupView() backupView {
	v := backupView{Configured: s.BackupDir != ""}
	if !v.Configured {
		return v
	}
	st, err := backup.Read(s.BackupDir)
	switch {
	case errors.Is(err, backup.ErrNone):
		return v
	case err != nil:
		v.Err = err.Error()
		return v
	}
	v.Status, v.Problem, v.LastGood = st, st.Problem(time.Now()), st.LastGood
	for _, fi := range backup.Files(s.BackupDir) {
		v.Files = append(v.Files, backupFile{Name: fi.Name(), Size: fi.Size(), At: fi.ModTime()})
	}
	if len(v.Files) > 0 {
		v.Latest = v.Files[0].Name
	}
	return v
}

func (s *Server) backupPage(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.render(w, http.StatusOK, "admin_backup.html", page{Title: "Backup", User: u, Data: s.backupView()})
}

// backupDownload sends one backup, as it is: encrypted with the passphrase of the backup service, which the controller does not
// have. Administrators only; the download is audited.
func (s *Server) backupDownload(w http.ResponseWriter, r *http.Request, u *auth.User) {
	name := r.PathValue("name")
	if s.BackupDir == "" {
		s.notFound(w, r, u)
		return
	}
	f, fi, err := backup.Open(s.BackupDir, name)
	if err != nil {
		s.notFound(w, r, u)
		return
	}
	defer f.Close()
	_ = audit.Record(r.Context(), s.Pool, audit.Entry{Action: "backup.download", ObjectType: "backup", ObjectID: name})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, f)
}

// backupConditions is the notification source for the backups: wrong when the last run failed or the last good backup is old;
// nothing at all when there is no backup service (a state file that is not there is not a failure).
func (s *Server) backupConditions(_ context.Context, now time.Time) ([]alerts.Condition, error) {
	if s.BackupDir == "" {
		return nil, nil
	}
	st, err := backup.Read(s.BackupDir)
	if errors.Is(err, backup.ErrNone) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("backup state: %w", err)
	}
	if p := st.Problem(now); p != "" {
		return []alerts.Condition{{Kind: alerts.KindBackup, Object: "backup", Who: alerts.Who{Admins: true}, Level: "danger", Title: p,
			Body: "A backup that cannot be restored is no backup. See Administration > Backup; the log of the backup service says why.", Href: "/admin/backup",
			OkTitle: "The backups are in order again"}}, nil
	}
	return []alerts.Condition{{Status: alerts.Fine, Kind: alerts.KindBackup, Object: "backup"}}, nil
}

// backupAttention is the line of the overview's attention list for the backups, or "".
func (s *Server) backupAttention() string {
	if s.BackupDir == "" {
		return ""
	}
	st, err := backup.Read(s.BackupDir)
	if err != nil { // no backup service, or a state this version cannot read: nothing to say here
		return ""
	}
	return st.Problem(time.Now())
}
