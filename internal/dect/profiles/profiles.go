// SPDX-License-Identifier: AGPL-3.0-or-later

// Package profiles is the catalogue of handset device profiles: the files of
// libopendectcell's profiles directory (docs/handset-profiles.md there), which say how a
// manufacturer's or a model's handsets differ from the standard. The stack
// takes the texts; this package only reads their headers for the lists the UI
// shows (a manufacturer, then a model, as in the LuCI app).
package profiles

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed data/*.profile
var files embed.FS

// Kind of a profile file.
const (
	Manufacturer = "manufacturer"
	Device       = "device"
)

// Profile is the header of one profile file.
type Profile struct {
	ID      string
	Kind    string
	Name    string
	Extends string // a device's manufacturer
	text    string
}

// Catalogue is the profiles by id.
type Catalogue struct {
	byID map[string]Profile
}

// Default is the catalogue built into the controller.
var Default = mustLoad(files, "data")

func mustLoad(fsys fs.FS, dir string) *Catalogue {
	c, err := Load(fsys, dir)
	if err != nil {
		panic("profiles: " + err.Error())
	}
	return c
}

// Load reads every .profile file of dir.
func Load(fsys fs.FS, dir string) (*Catalogue, error) {
	ents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	c := &Catalogue{byID: map[string]Profile{}}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".profile") {
			continue
		}
		b, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		p := Profile{text: string(b)}
		for _, l := range strings.Split(p.text, "\n") {
			k, v, ok := strings.Cut(l, "=")
			if !ok {
				continue
			}
			v, _, _ = strings.Cut(v, "#")
			v = strings.Trim(strings.TrimSpace(v), `"`)
			switch strings.TrimSpace(k) {
			case "id":
				p.ID = v
			case "kind":
				p.Kind = v
			case "name":
				p.Name = v
			case "extends":
				p.Extends = v
			}
		}
		if p.ID == "" || p.ID+".profile" != e.Name() || (p.Kind != Manufacturer && p.Kind != Device) {
			continue
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		c.byID[p.ID] = p
	}
	return c, nil
}

// Get is the profile with this id.
func (c *Catalogue) Get(id string) (Profile, bool) {
	p, ok := c.byID[id]
	return p, ok
}

// Manufacturers are the manufacturers, by name.
func (c *Catalogue) Manufacturers() []Profile {
	return c.list(func(p Profile) bool { return p.Kind == Manufacturer })
}

// Models are a manufacturer's devices, by name.
func (c *Catalogue) Models(mfr string) []Profile {
	return c.list(func(p Profile) bool { return p.Kind == Device && p.Extends == mfr })
}

func (c *Catalogue) list(keep func(Profile) bool) []Profile {
	var out []Profile
	for _, p := range c.byID {
		if keep(p) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Texts are the files the stack needs for a profile id: the manufacturer's
// and the device's (either may be empty). ok is false for an unknown id; ""
// is the standard behaviour, no files.
func (c *Catalogue) Texts(id string) (mfr, dev string, ok bool) {
	if id == "" {
		return "", "", true
	}
	p, ok := c.byID[id]
	if !ok {
		return "", "", false
	}
	if p.Kind == Manufacturer {
		return p.text, "", true
	}
	if m, found := c.byID[p.Extends]; found {
		mfr = m.text
	}
	return mfr, p.text, true
}

// Label is what the UI shows for an id: the model's name, or "" for none.
func (c *Catalogue) Label(id string) string {
	if p, ok := c.byID[id]; ok {
		return p.Name
	}
	return ""
}
