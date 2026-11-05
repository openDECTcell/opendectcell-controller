// SPDX-License-Identifier: AGPL-3.0-or-later

package transfer

import (
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
)

// Limits of one file: a setup that is larger is not a person's setup.
const (
	MaxHandsets = 500
	MaxLines    = 200
)

// Setup is what a file holds. Every part is optional.
type Setup struct {
	Created  time.Time `json:"created"`
	Handsets []Handset `json:"handsets,omitempty"`
	Lines    []Line    `json:"lines,omitempty"`
	// Contacts is the user's own contacts as a vCard 3.0 text (address-book contacts are not in it: they come from their server).
	Contacts string `json:"contacts_vcf,omitempty"`
}

// Handset is one handset. A handset with a Key can be created by an import; one without only brings its settings to a handset
// of the same identity the user has already.
type Handset struct {
	Name           string            `json:"name"`
	Model          string            `json:"model,omitempty"`
	Profile        string            `json:"profile,omitempty"`
	IPUI           string            `json:"ipui"` // hexadecimal
	EMC            *int              `json:"emc,omitempty"`
	DefaultLine    *int              `json:"default_line,omitempty"`
	DualRing       string            `json:"dual_ring,omitempty"`
	InternalNumber *int              `json:"internal_number,omitempty"`
	Lines          map[string]string `json:"lines,omitempty"` // "1" and "2" to the Ref of a Line of this file
	Key            *Key              `json:"key,omitempty"`
}

// Key is the secret of a handset: what the base station and the handset share. Hexadecimal.
type Key struct {
	UAK       string `json:"uak"`
	CCPortID  string `json:"cc_port_id,omitempty"`
	CCFixedID string `json:"cc_fixed_id,omitempty"`
}

// Line is one VoIP line. Ref is how the handsets of the file name it; it means nothing outside the file.
type Line struct {
	Ref             string              `json:"ref"`
	Label           string              `json:"label"`
	DisplayName     string              `json:"display_name,omitempty"`
	Username        string              `json:"username"`
	Domain          string              `json:"domain"`
	AuthUsername    string              `json:"auth_username,omitempty"`
	Registrar       string              `json:"registrar,omitempty"`
	OutboundProxy   string              `json:"outbound_proxy,omitempty"`
	Transport       string              `json:"transport"`
	Register        bool                `json:"register"`
	RegisterExpiry  int                 `json:"register_expiry"`
	Keepalive       string              `json:"keepalive"`
	MediaEncryption string              `json:"media_encryption"`
	Codecs          []string            `json:"codecs"`
	DTMFMode        string              `json:"dtmf_mode"`
	CallerIDSource  string              `json:"caller_id_source"`
	TLSVerify       bool                `json:"tls_verify"`
	Enabled         bool                `json:"enabled"`
	Advanced        sipaccount.Advanced `json:"advanced"`
	Password        string              `json:"password,omitempty"`
	StunPassword    string              `json:"turn_password,omitempty"`
}

// HasSecrets says whether the setup carries a handset key or a password.
func (s Setup) HasSecrets() bool {
	for _, h := range s.Handsets {
		if h.Key != nil {
			return true
		}
	}
	for _, l := range s.Lines {
		if l.Password != "" || l.StunPassword != "" {
			return true
		}
	}
	return false
}
