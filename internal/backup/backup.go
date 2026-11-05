// SPDX-License-Identifier: AGPL-3.0-or-later

// Package backup reads what the backup service (deploy/backup) leaves in the backup directory: the state of its last run, and the
// backups. The service makes them; the controller only shows them, serves the newest to an administrator and warns when they are
// wrong or old. It never writes to the directory (it is mounted read only).
package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// MaxAge is how old the last good backup may be before the administrators are told: a daily backup that missed one night is
// still fine, one that missed two is not.
const MaxAge = 36 * time.Hour

// ErrNone means the backup service has not written a state: it is not part of this stack, or it has not started.
var ErrNone = errors.New("backup: no state file")

// Status is the content of status.json.
type Status struct {
	Version  int    `json:"version"`
	OK       bool   `json:"ok"`
	Started  string `json:"started"`
	Finished string `json:"finished"`
	File     string `json:"file"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Error    string `json:"error"`
	Check    struct {
		Ran    bool   `json:"ran"`
		OK     bool   `json:"ok"`
		Users  int    `json:"users"`
		Tables int    `json:"tables"`
		Error  string `json:"error"`
	} `json:"check"`
	SFTP struct {
		Configured bool   `json:"configured"`
		OK         bool   `json:"ok"`
		Error      string `json:"error"`
	} `json:"sftp"`
	Keep int    `json:"keep"`
	Next string `json:"next"`
	At   string `json:"at"` // when the file was written

	// LastGood is the finish time of the newest backup that was made and held (filled in by Read from the files).
	LastGood time.Time `json:"-"`
}

// Read returns the state in dir. ErrNone if there is none. The file is small: more than 64 KiB is not ours.
func Read(dir string) (*Status, error) {
	f, err := os.Open(filepath.Join(dir, "status.json")) //nolint:gosec // G304: the operator's own backup directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNone
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10)) // a longer file is cut, and a cut JSON does not parse
	if err != nil {
		return nil, errors.New("backup: the state file is not readable")
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil || s.Version != 1 {
		return nil, errors.New("backup: the state file is not one this version knows")
	}
	s.LastGood = lastGood(dir, &s)
	return &s, nil
}

// lastGood is when the newest backup was made, and held, as far as the state says: this run's finish time if it was good, else the
// time of the newest backup file (a failed run after good ones leaves the good files).
func lastGood(dir string, s *Status) time.Time {
	if s.OK && s.Finished != "" {
		if t, err := time.Parse(time.RFC3339, s.Finished); err == nil {
			return t
		}
	}
	names, _ := filepath.Glob(filepath.Join(dir, "opendectcell-*.dump.gpg"))
	var newest time.Time
	for _, n := range names {
		if !fileName.MatchString(filepath.Base(n)) {
			continue
		}
		if fi, err := os.Lstat(n); err == nil && fi.Mode().IsRegular() && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// Problem says what is wrong with the backups as of now, or "" if nothing is: the last run failed, or the last good backup is too
// old (or there is none and the service has been running a while).
func (s *Status) Problem(now time.Time) string {
	switch {
	case s.Finished == "": // the service has started and has not made one yet: a day is its schedule, a day and a half is a problem
		if at, err := time.Parse(time.RFC3339, s.At); err == nil && now.Sub(at) > MaxAge {
			return fmt.Sprintf("No backup has been made yet, and the backup service started %d hours ago", int(now.Sub(at).Hours()))
		}
		return ""
	case !s.OK:
		msg := "The last backup failed"
		if s.Error != "" {
			msg += ": " + s.Error
		}
		return msg
	case !s.LastGood.IsZero() && now.Sub(s.LastGood) > MaxAge:
		return fmt.Sprintf("The last good backup is %d hours old", int(now.Sub(s.LastGood).Hours()))
	}
	return ""
}

// The names the service makes: opendectcell-YYYYMMDD-HHMMSS.dump.gpg.
var fileName = regexp.MustCompile(`^opendectcell-[0-9]{8}-[0-9]{6}\.dump\.gpg$`)

// ValidName says whether name is one of the names the service makes.
func ValidName(name string) bool { return fileName.MatchString(name) }

// Open opens the backup called name in dir for download: a regular file of the names the service makes, nothing else (no path, no
// link that leads out of the directory).
func Open(dir, name string) (*os.File, os.FileInfo, error) {
	if !ValidName(name) {
		return nil, nil, os.ErrNotExist
	}
	p := filepath.Join(dir, name)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, os.ErrNotExist
	}
	f, err := os.Open(p) //nolint:gosec // G304: the name is checked against the pattern above
	if err != nil {
		return nil, nil, err
	}
	return f, fi, nil
}

// Files lists the backups in dir, newest first.
func Files(dir string) []os.FileInfo {
	names, _ := filepath.Glob(filepath.Join(dir, "opendectcell-*.dump.gpg"))
	var out []os.FileInfo
	for _, n := range names {
		if fi, err := os.Lstat(n); err == nil && fi.Mode().IsRegular() && ValidName(fi.Name()) {
			out = append(out, fi)
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Name() > out[i].Name() {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
