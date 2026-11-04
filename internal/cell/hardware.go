// SPDX-License-Identifier: AGPL-3.0-or-later

package cell

import (
	"context"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
)

// ModelName is the hardware model a cell reports ("AVM FRITZ!Box 7520") as the pages name it: without the
// maker's prefix, which every box would repeat.
func ModelName(hw string) string {
	hw = strings.TrimSpace(hw)
	if len(hw) > 4 && strings.EqualFold(hw[:4], "AVM ") {
		hw = strings.TrimSpace(hw[4:])
	}
	return hw
}

// Detected is the model the base station reported itself; empty for one that has not connected since the
// controller learnt to keep it.
func (c *Cell) Detected() string { return ModelName(c.HWModel) }

// Model is what the pages show as the hardware: what the owner entered, else what the box reported, else "".
func (c *Cell) Model() string {
	if m := strings.TrimSpace(c.BoxModel); m != "" {
		return m
	}
	return c.Detected()
}

// DefaultHostname is the name a box has before anyone names it: OpenWrt's default hostname.
const DefaultHostname = "OpenWrt"

// genericName matches that hostname and the "-2" and "-3" the controller adds when it is taken. Whether such a
// name is the box's default or somebody's choice is name_custom's to say, which enrolment sets from the hostname
// the box reported.
var genericName = regexp.MustCompile(`(?i)^openwrt(-\d+)?$`)

// GenericName reports whether name is one nobody chose.
func GenericName(name string) bool { return genericName.MatchString(strings.TrimSpace(name)) }

// AutoName is the name for a base station whose owner left the default: its model and the last five digits of
// the identity it broadcasts (the RFPI, ten hexadecimal digits), "FRITZ!Box 7520 72a00". "" when either is
// missing.
func AutoName(model string, rfpi []byte) string {
	if model == "" || len(rfpi) != 5 {
		return ""
	}
	h := hex.EncodeToString(rfpi)
	return model + " " + h[len(h)-5:]
}

// autoName gives a base station that still carries the default name the name AutoName makes, once the model and
// the identity are known. A name the owner chose (name_custom) or any other name is left alone. The caller holds
// the transaction.
func (s *Store) autoName(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var name, boxModel, hw string
	var custom bool
	var rfpi []byte
	if err := tx.QueryRow(ctx, "SELECT name, name_custom, box_model, hw_model, rfpi FROM cell WHERE id = $1 FOR UPDATE", id).
		Scan(&name, &custom, &boxModel, &hw, &rfpi); err != nil {
		return err
	}
	if custom || !GenericName(name) {
		return nil
	}
	c := Cell{Profile: Profile{BoxModel: boxModel}, HWModel: hw}
	want := AutoName(c.Model(), rfpi)
	if want == "" {
		return nil
	}
	unique, err := s.uniqueName(ctx, tx, want)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE cell SET name = $2 WHERE id = $1", id, unique); err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Entry{Action: "cell.rename_auto", ObjectType: "cell", ObjectID: id.String(),
		Details: map[string]any{"from": name, "to": unique}})
}
