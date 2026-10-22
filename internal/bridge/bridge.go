// SPDX-License-Identifier: AGPL-3.0-or-later

// Package bridge is where a handset's DECT session becomes a SIP call
// (opendectcell-controller#48). It is the one place that decides, from the
// <<BASIC-SERVICE>> call class, whether a session is a call at all: CAT-iq
// list access and other data sessions complete the same CC setup/connect
// sequence and must never reach SIP or get audio.
//
// The DECT core (C3, frozen by #46) will call Originate for every
// handset-originated {CC-SETUP}; until then the tests stand in for it.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/callclass"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
)

// Setup is a handset-originated call setup, as the DECT core sees it.
type Setup struct {
	HandsetID uuid.UUID
	AccountID uuid.UUID // the SIP account of the line the call goes out on
	Number    string
	Class     callclass.Class
	Service   callclass.Service
}

// Dialer is the SIP side (sip.Manager).
type Dialer interface {
	Dial(ctx context.Context, accountID uuid.UUID, number string) (*sip.Call, error)
}

// ErrNotACall means the session is not a voice call (list access, ULE,
// SUOTA, …). The DECT core handles such sessions itself; no SIP leg and no
// audio channel exist for them.
var ErrNotACall = errors.New("bridge: not a voice call")

// ErrNotYet means a call class that is recognised but not bridged yet
// (internal calls, 0x9).
var ErrNotYet = errors.New("bridge: call class not supported yet")

// Bridge turns handset setups into SIP calls.
type Bridge struct {
	SIP Dialer
	Log *slog.Logger
}

func (b *Bridge) log() *slog.Logger {
	if b.Log == nil {
		return slog.Default()
	}
	return b.Log
}

// Originate dials SIP for a handset-originated setup, if and only if its
// call class is a voice call (callclass.Bridge). Anything else is refused
// and logged, never bridged optimistically. The caller must not assign an
// audio channel unless Originate succeeded.
func (b *Bridge) Originate(ctx context.Context, s Setup) (*sip.Call, error) {
	switch {
	case callclass.Bridge(s.Class):
	case s.Class == callclass.Internal:
		b.log().Info("internal call refused: not supported yet", "handset", s.HandsetID)
		return nil, ErrNotYet
	default:
		// Not an error on the handset's part: list access and the like are
		// normal. Debug, not warning: an A540 does this several times per call.
		b.log().Debug("DECT session is not a call; not bridged", "handset", s.HandsetID, "class", s.Class.String())
		return nil, fmt.Errorf("%w: %s", ErrNotACall, s.Class)
	}
	return b.SIP.Dial(ctx, s.AccountID, s.Number)
}
