// SPDX-License-Identifier: AGPL-3.0-or-later

package stats

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// FleetCell is one active base station as the fleet view compares them (administrators).
type FleetCell struct {
	ID          uuid.UUID
	Name        string
	Hardware    string // what it reports about itself, or what its owner entered
	Software    string // the cell software's version, "" until it has reported
	Stub        string // the firmware stub's version
	LastSeenAt  *time.Time
	Calls       int   // calls it carried in the range, a call that moved counts on each
	TalkSeconds int64 // their conversation time
	Handsets    int   // handsets heard on it in the last 7 days
	Behind      bool  // its software is older than the newest in the fleet (filled in by FleetCells)
}

// FleetCells returns the active base stations with their load over the last days and the handsets heard on each in the last 7 days,
// the busiest first. A base station without a call is in it, with 0.
func (s *Store) FleetCells(ctx context.Context, days int) ([]FleetCell, error) {
	now := s.now()
	rows, err := s.Pool.Query(ctx, `SELECT c.id, c.name, coalesce(nullif(c.hw_model, ''), nullif(c.box_model, ''), ''), coalesce(c.software_version, ''), coalesce(c.stub_version, ''), c.last_seen_at,
			(SELECT count(*) FROM call_log l WHERE c.id = ANY(l.cell_ids) AND l.started_at >= $1),
			(SELECT coalesce(sum(extract(epoch FROM l.ended_at - l.answered_at)), 0)::bigint FROM call_log l
				WHERE c.id = ANY(l.cell_ids) AND l.started_at >= $1 AND l.answered_at IS NOT NULL AND l.ended_at IS NOT NULL),
			(SELECT count(*) FROM (SELECT m.handset_id FROM handset_move m WHERE m.to_cell_id = c.id AND m.at >= $2
				UNION SELECT h.id FROM handset h WHERE h.last_seen_cell_id = c.id AND h.last_seen_at >= $2) heard)
		FROM cell c WHERE c.state = 'active' ORDER BY 7 DESC, lower(c.name), c.id`, now.AddDate(0, 0, -days), now.AddDate(0, 0, -7))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FleetCell
	for rows.Next() {
		var c FleetCell
		if err := rows.Scan(&c.ID, &c.Name, &c.Hardware, &c.Software, &c.Stub, &c.LastSeenAt, &c.Calls, &c.TalkSeconds, &c.Handsets); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	newest := ""
	for _, c := range out {
		if c.Software != "" && (newest == "" || CompareVersions(c.Software, newest) > 0) {
			newest = c.Software
		}
	}
	for i := range out {
		out[i].Behind = out[i].Software != "" && newest != "" && CompareVersions(out[i].Software, newest) < 0
	}
	return out, nil
}

// VersionGroup is how many base stations run one software version.
type VersionGroup struct {
	Version string // "" for the ones that have not reported
	Count   int
	Newest  bool
	Behind  bool
}

// VersionSpread counts the base stations by software version, the newest first and the ones that have not reported last.
func VersionSpread(cells []FleetCell) []VersionGroup {
	count := map[string]int{}
	for _, c := range cells {
		count[c.Software]++
	}
	var out []VersionGroup
	for v, n := range count {
		out = append(out, VersionGroup{Version: v, Count: n})
	}
	slices.SortFunc(out, func(a, b VersionGroup) int {
		switch {
		case a.Version == "" && b.Version != "":
			return 1
		case a.Version != "" && b.Version == "":
			return -1
		}
		return -CompareVersions(a.Version, b.Version)
	})
	for i := range out {
		out[i].Newest = i == 0 && out[i].Version != ""
		out[i].Behind = out[i].Version != "" && !out[i].Newest
	}
	return out
}

// CompareVersions orders two version strings: numbers are compared as numbers, so 0.10 is later than 0.9, and a text part as text. A
// version that is a prefix of another (0.3 and 0.3.1) is the earlier one. It returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, y := pa[i], pb[i]
		nx, ex := strconv.Atoi(x)
		ny, ey := strconv.Atoi(y)
		var c int
		switch {
		case ex == nil && ey == nil:
			c = cmp.Compare(nx, ny)
		case ex == nil: // a number is earlier than text in the same place (1.0 before 1.0rc is not meant: text rarely appears)
			c = -1
		case ey == nil:
			c = 1
		default:
			c = strings.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(pa), len(pb))
}

// versionParts splits a version at every change between digits, letters and separators: "0.3.4-rc1" is 0 3 4 rc 1.
func versionParts(v string) []string {
	var parts []string
	var cur []rune
	kind := 0 // 1 digits, 2 letters
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), "v") {
		k := 0
		switch {
		case unicode.IsDigit(r):
			k = 1
		case unicode.IsLetter(r):
			k = 2
		}
		if k == 0 || k != kind {
			flush()
		}
		if k != 0 {
			cur = append(cur, r)
		}
		kind = k
	}
	flush()
	return parts
}
