// SPDX-License-Identifier: AGPL-3.0-or-later

package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/callclass"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
)

// dialer records whether SIP was dialled. A real Dial also opens the RTP
// endpoint, so "not dialled" means no SIP leg and no RTP.
type dialer struct{ calls int }

func (d *dialer) Dial(context.Context, uuid.UUID, string) (*sip.Call, error) {
	d.calls++
	return &sip.Call{}, nil
}

func TestOnlyVoiceCallsReachSIP(t *testing.T) {
	for _, c := range []struct {
		octet uint8
		want  error
		dial  bool
	}{
		{0x88, nil, true},          // normal wideband call
		{0x80, nil, true},          // normal narrowband call
		{0xA0, nil, true},          // emergency: never blocked
		{0x28, ErrNotACall, false}, // CAT-iq list access: no SIP leg, no RTP
		{0x38, ErrNotACall, false}, // ULE
		{0xB0, ErrNotACall, false}, // service call
		{0x0F, ErrNotACall, false}, // unrecognised class
		{0x98, ErrNotYet, false},   // internal: recognised, reserved
	} {
		d := &dialer{}
		b := &Bridge{SIP: d}
		cl, sv := callclass.Parse(c.octet)
		_, err := b.Originate(context.Background(), Setup{Class: cl, Service: sv, Number: "123"})
		if !errors.Is(err, c.want) {
			t.Errorf("0x%02X: err %v, want %v", c.octet, err, c.want)
		}
		if (d.calls > 0) != c.dial {
			t.Errorf("0x%02X: dialled %d times", c.octet, d.calls)
		}
	}
}
