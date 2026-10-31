// SPDX-License-Identifier: AGPL-3.0-or-later

package wire

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// ProtoMajor and ProtoMinor are the protocol version this package speaks.
const (
	ProtoMajor = 1
	ProtoMinor = 0
)

// Capability bits (§6.1).
const (
	CapMail uint32 = 1 << iota
	CapMedia
	CapSRTP
	CapPlainRTP
	CapStats
	CapFlow
	CapResume
	CapLog
	CapUpdate
	CapDebug
)

// CellStateValue is a cell state (§6.2).
type CellStateValue uint8

const (
	StatePending CellStateValue = iota
	StateActive
	StateDisabled
	StateRevoked
)

var stateNames = [...]string{"pending", "active", "disabled", "revoked"}

func (s CellStateValue) String() string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return fmt.Sprintf("state(%d)", uint8(s))
}

// ParseCellState maps the database name to the wire value.
func ParseCellState(name string) (CellStateValue, bool) {
	for i, n := range stateNames {
		if n == name {
			return CellStateValue(i), true
		}
	}
	return 0, false
}

// HelloMsg is HELLO (§6.1).
type HelloMsg struct {
	Major, Minor     uint8
	SWVersion        string
	StubVersion      string
	HWModel          string
	BootID           [16]byte
	ChipEpoch        uint32
	Capabilities     uint32
	CapSubRecords    uint8
	CapRadioContexts uint8
	CapAudioNB       uint8
	CapAudioWB       uint8
	AudioFormats     []uint8 // tag 12: what the cell accepts (downlink), preferred first
	AudioFormatsUp   []uint8 // tag 16: what it sends (uplink); nil = same as AudioFormats
	ResumeToken      []byte  // nil or 16 bytes
	RadioState       uint8
	CellTimeMs       uint64 // 0 if absent
	// RFPI is the 40-bit identity the cell's chip broadcasts (tag 17), if
	// the cell reports it; HasRFPI says it did.
	RFPI    [5]byte
	HasRFPI bool
}

func (m HelloMsg) Encode() []byte {
	w := new(Builder).Bytes(1, []byte{m.Major, m.Minor}).String(2, m.SWVersion)
	if m.StubVersion != "" {
		w.String(3, m.StubVersion)
	}
	if m.HWModel != "" {
		w.String(4, m.HWModel)
	}
	w.Bytes(5, m.BootID[:]).U32(6, m.ChipEpoch).U32(7, m.Capabilities).
		U8(8, m.CapSubRecords).U8(9, m.CapRadioContexts).U8(10, m.CapAudioNB).U8(11, m.CapAudioWB)
	if len(m.AudioFormats) > 0 {
		w.Bytes(12, m.AudioFormats)
	}
	if m.ResumeToken != nil {
		w.Bytes(13, m.ResumeToken)
	}
	w.U8(14, m.RadioState)
	if m.CellTimeMs != 0 {
		w.U64(15, m.CellTimeMs)
	}
	if len(m.AudioFormatsUp) > 0 {
		w.Bytes(16, m.AudioFormatsUp)
	}
	if m.HasRFPI {
		w.Bytes(17, m.RFPI[:])
	}
	return w.Build()
}

// DecodeHello parses HELLO. The version fields are filled first, so a
// caller can still tell a major-version mismatch from a returned message
// whose later tags were malformed.
func DecodeHello(b []byte) (HelloMsg, error) {
	var m HelloMsg
	t, err := ParseTLV(b)
	if err != nil {
		return m, err
	}
	v, err := t.Fixed(1, 2)
	if err != nil {
		return m, err
	}
	m.Major, m.Minor = v[0], v[1]
	if m.SWVersion, err = t.Str(2, 64); err != nil {
		return m, err
	}
	if m.StubVersion, err = t.OptStr(3, 64); err != nil {
		return m, err
	}
	if m.HWModel, err = t.OptStr(4, 64); err != nil {
		return m, err
	}
	id, err := t.Fixed(5, 16)
	if err != nil {
		return m, err
	}
	copy(m.BootID[:], id)
	if m.ChipEpoch, err = t.U32(6); err != nil {
		return m, err
	}
	if m.Capabilities, err = t.U32(7); err != nil {
		return m, err
	}
	for tag, p := range map[uint8]*uint8{8: &m.CapSubRecords, 9: &m.CapRadioContexts, 10: &m.CapAudioNB, 11: &m.CapAudioWB, 14: &m.RadioState} {
		if *p, err = t.U8(tag); err != nil {
			return m, err
		}
	}
	if f, ok := t.Get(12); ok {
		if len(f) > 16 {
			return m, fmt.Errorf("%w: more than 16 audio formats", ErrMalformed)
		}
		m.AudioFormats = append([]uint8(nil), f...)
	} else if m.Capabilities&CapMedia != 0 {
		return m, fmt.Errorf("%w: MEDIA without audio_formats", ErrMalformed)
	}
	if t.Has(13) {
		if m.ResumeToken, err = t.Fixed(13, 16); err != nil {
			return m, err
		}
	}
	if t.Has(15) {
		if m.CellTimeMs, err = t.U64(15); err != nil {
			return m, err
		}
	}
	if f, ok := t.Get(16); ok {
		if len(f) == 0 || len(f) > 16 {
			return m, fmt.Errorf("%w: audio_formats_up must list 1 to 16 formats", ErrMalformed)
		}
		m.AudioFormatsUp = append([]uint8(nil), f...)
	}
	if t.Has(17) {
		r, err := t.Fixed(17, 5)
		if err != nil {
			return m, err
		}
		copy(m.RFPI[:], r)
		m.HasRFPI = true
	}
	return m, nil
}

// HelloAckMsg is HELLO_ACK (§6.1).
type HelloAckMsg struct {
	Major, Minor     uint8
	CellID           [16]byte
	State            CellStateValue
	SessionToken     [16]byte
	Resumed          bool
	HeartbeatMs      uint16
	DeadMs           uint16
	GraceMs          uint16
	OrphanS          uint16
	Capabilities     uint32
	ControllerTimeMs uint64
}

func (m HelloAckMsg) Encode() []byte {
	r := uint8(0)
	if m.Resumed {
		r = 1
	}
	return new(Builder).Bytes(1, []byte{m.Major, m.Minor}).Bytes(2, m.CellID[:]).U8(3, uint8(m.State)).
		Bytes(4, m.SessionToken[:]).U8(5, r).U16(6, m.HeartbeatMs).U16(7, m.DeadMs).U16(8, m.GraceMs).
		U16(9, m.OrphanS).U32(10, m.Capabilities).U64(11, m.ControllerTimeMs).Build()
}

func DecodeHelloAck(b []byte) (HelloAckMsg, error) {
	var m HelloAckMsg
	t, err := ParseTLV(b)
	if err != nil {
		return m, err
	}
	v, err := t.Fixed(1, 2)
	if err != nil {
		return m, err
	}
	m.Major, m.Minor = v[0], v[1]
	id, err := t.Fixed(2, 16)
	if err != nil {
		return m, err
	}
	copy(m.CellID[:], id)
	st, err := t.U8(3)
	if err != nil {
		return m, err
	}
	m.State = CellStateValue(st)
	tok, err := t.Fixed(4, 16)
	if err != nil {
		return m, err
	}
	copy(m.SessionToken[:], tok)
	r, err := t.U8(5)
	if err != nil {
		return m, err
	}
	m.Resumed = r == 1
	for tag, p := range map[uint8]*uint16{6: &m.HeartbeatMs, 7: &m.DeadMs, 8: &m.GraceMs, 9: &m.OrphanS} {
		if *p, err = t.U16(tag); err != nil {
			return m, err
		}
	}
	if m.Capabilities, err = t.U32(10); err != nil {
		return m, err
	}
	m.ControllerTimeMs, err = t.U64(11)
	return m, err
}

// EnrollRequestMsg is ENROLL_REQUEST (§5.2).
type EnrollRequestMsg struct {
	Token     []byte // 20 bytes
	CSR       []byte // DER
	HWModel   string
	SWVersion string
	NameHint  string
}

// TokenLen is the raw enrollment token length (§5.1).
const TokenLen = 20

func (m EnrollRequestMsg) Encode() []byte {
	w := new(Builder).Bytes(1, m.Token).Bytes(2, m.CSR)
	if m.HWModel != "" {
		w.String(3, m.HWModel)
	}
	w.String(4, m.SWVersion)
	if m.NameHint != "" {
		w.String(5, m.NameHint)
	}
	return w.Build()
}

func DecodeEnrollRequest(b []byte) (EnrollRequestMsg, error) {
	var m EnrollRequestMsg
	t, err := ParseTLV(b)
	if err != nil {
		return m, err
	}
	if m.Token, err = t.Fixed(1, TokenLen); err != nil {
		return m, err
	}
	csr, err := need(t, 2, -1)
	if err != nil {
		return m, err
	}
	if len(csr) > 1024 {
		return m, fmt.Errorf("%w: CSR longer than 1024 bytes", ErrMalformed)
	}
	m.CSR = csr
	if m.HWModel, err = t.OptStr(3, 64); err != nil {
		return m, err
	}
	if m.SWVersion, err = t.Str(4, 64); err != nil {
		return m, err
	}
	m.NameHint, err = t.OptStr(5, 64)
	return m, err
}

// Enrollment statuses (§5.2).
const (
	EnrollOK            uint16 = 0
	EnrollTokenRejected uint16 = 1
	EnrollBadCSR        uint16 = 2
	EnrollRateLimited   uint16 = 3
	EnrollInternal      uint16 = 4
)

// EnrollResponseMsg is ENROLL_RESPONSE.
type EnrollResponseMsg struct {
	Status uint16
	CellID [16]byte
	Cert   []byte
	CACert []byte
	State  CellStateValue
}

func (m EnrollResponseMsg) Encode() []byte {
	w := new(Builder).U16(1, m.Status)
	if m.Status == EnrollOK {
		w.Bytes(2, m.CellID[:]).Bytes(3, m.Cert).Bytes(4, m.CACert).U8(5, uint8(m.State))
	}
	return w.Build()
}

func DecodeEnrollResponse(b []byte) (EnrollResponseMsg, error) {
	var m EnrollResponseMsg
	t, err := ParseTLV(b)
	if err != nil {
		return m, err
	}
	if m.Status, err = t.U16(1); err != nil || m.Status != EnrollOK {
		return m, err
	}
	id, err := t.Fixed(2, 16)
	if err != nil {
		return m, err
	}
	copy(m.CellID[:], id)
	if m.Cert, err = need(t, 3, -1); err != nil {
		return m, err
	}
	if m.CACert, err = need(t, 4, -1); err != nil {
		return m, err
	}
	st, err := t.U8(5)
	m.State = CellStateValue(st)
	return m, err
}

// Goodbye reasons (§11).
const (
	ByeShutdown   uint16 = 1
	ByeProtocol   uint16 = 2
	ByeVersion    uint16 = 3
	ByeRevoked    uint16 = 4
	ByeSuperseded uint16 = 5
	ByeOverload   uint16 = 6
	ByeAuth       uint16 = 7
)

// GoodbyeMsg is GOODBYE.
type GoodbyeMsg struct {
	Reason uint16
	Text   string
}

func (m GoodbyeMsg) Encode() []byte {
	w := new(Builder).U16(1, m.Reason)
	if m.Text != "" {
		w.String(2, truncate(m.Text, 128))
	}
	return w.Build()
}

func DecodeGoodbye(b []byte) (GoodbyeMsg, error) {
	var m GoodbyeMsg
	t, err := ParseTLV(b)
	if err != nil {
		return m, err
	}
	if m.Reason, err = t.U16(1); err != nil {
		return m, err
	}
	m.Text, err = t.OptStr(2, 128)
	return m, err
}

// Error codes (§11).
const (
	ErrUnknownType uint16 = 1
	ErrMalformedC  uint16 = 2
	ErrTooLarge    uint16 = 3
	ErrNotAllowed  uint16 = 4
	ErrUnsupported uint16 = 5
	ErrBusy        uint16 = 6
	ErrMediaFailed uint16 = 7
	ErrInternal    uint16 = 8
)

// ErrorMsg is ERROR. Text must never contain secrets or identities.
type ErrorMsg struct {
	Code    uint16
	RefType Type
	RefCorr uint16
	Text    string
}

func (m ErrorMsg) Encode() []byte {
	w := new(Builder).U16(1, m.Code)
	if m.RefType != 0 {
		w.U16(2, uint16(m.RefType))
	}
	if m.RefCorr != 0 {
		w.U16(3, m.RefCorr)
	}
	if m.Text != "" {
		w.String(4, truncate(m.Text, 128))
	}
	return w.Build()
}

func DecodeError(b []byte) (ErrorMsg, error) {
	var m ErrorMsg
	t, err := ParseTLV(b)
	if err != nil {
		return m, err
	}
	if m.Code, err = t.U16(1); err != nil {
		return m, err
	}
	if t.Has(2) {
		v, err := t.U16(2)
		if err != nil {
			return m, err
		}
		m.RefType = Type(v)
	}
	if t.Has(3) {
		if m.RefCorr, err = t.U16(3); err != nil {
			return m, err
		}
	}
	m.Text, err = t.OptStr(4, 128)
	return m, err
}

// CellStateMsg is CELL_STATE (§6.2).
type CellStateMsg struct {
	State  CellStateValue
	Reason string
}

func (m CellStateMsg) Encode() []byte {
	w := new(Builder).U8(1, uint8(m.State))
	if m.Reason != "" {
		w.String(2, truncate(m.Reason, 128))
	}
	return w.Build()
}

func DecodeCellState(b []byte) (CellStateMsg, error) {
	var m CellStateMsg
	t, err := ParseTLV(b)
	if err != nil {
		return m, err
	}
	st, err := t.U8(1)
	if err != nil {
		return m, err
	}
	m.State = CellStateValue(st)
	m.Reason, err = t.OptStr(2, 128)
	return m, err
}

// StatusMsg is STATUS (§10). Fields are optional; Has* report presence.
type StatusMsg struct {
	RadioState, ChipState uint8
	ChipEpoch, UptimeS    uint32
	Detail                string
	HasRadio, HasChip     bool
	// RFPI is the identity the chip broadcasts (tag 6), if reported.
	RFPI    [5]byte
	HasRFPI bool
}

func (m StatusMsg) Encode() []byte {
	w := new(Builder)
	if m.HasRadio {
		w.U8(1, m.RadioState)
	}
	if m.HasChip {
		w.U8(2, m.ChipState)
	}
	w.U32(3, m.ChipEpoch).U32(4, m.UptimeS)
	if m.Detail != "" {
		w.String(5, truncate(m.Detail, 128))
	}
	if m.HasRFPI {
		w.Bytes(6, m.RFPI[:])
	}
	return w.Build()
}

func DecodeStatus(b []byte) (StatusMsg, error) {
	var m StatusMsg
	t, err := ParseTLV(b)
	if err != nil {
		return m, err
	}
	if t.Has(1) {
		if m.RadioState, err = t.U8(1); err != nil {
			return m, err
		}
		m.HasRadio = true
	}
	if t.Has(2) {
		if m.ChipState, err = t.U8(2); err != nil {
			return m, err
		}
		m.HasChip = true
	}
	if t.Has(3) {
		if m.ChipEpoch, err = t.U32(3); err != nil {
			return m, err
		}
	}
	if t.Has(4) {
		if m.UptimeS, err = t.U32(4); err != nil {
			return m, err
		}
	}
	if m.Detail, err = t.OptStr(5, 128); err != nil {
		return m, err
	}
	if t.Has(6) {
		r, err := t.Fixed(6, 5)
		if err != nil {
			return m, err
		}
		copy(m.RFPI[:], r)
		m.HasRFPI = true
	}
	return m, nil
}

// ConfigSetMsg is CONFIG_SET (§10). Only the DECT identity is built here;
// the cell applies what it understands.
type ConfigSetMsg struct {
	// RFPI, if HasRFPI, is the identity the cell's chip is to broadcast
	// (tag 6): class A or B, E bit 0. A change reloads the chip.
	RFPI    [5]byte
	HasRFPI bool
	// Revert asks the cell to go back to its own identity: tag 6 empty.
	Revert bool
}

func (m ConfigSetMsg) Encode() []byte {
	w := new(Builder)
	switch {
	case m.HasRFPI:
		w.Bytes(6, m.RFPI[:])
	case m.Revert:
		w.Bytes(6, nil)
	}
	return w.Build()
}

// DecodeConfigAck returns CONFIG_ACK's status: 0 everything applied, 1 only
// part of it.
func DecodeConfigAck(b []byte) (uint16, error) {
	t, err := ParseTLV(b)
	if err != nil {
		return 0, err
	}
	return t.U16(1)
}

// Stats counter ids (§10).
var StatsCounterNames = map[uint16]string{
	1: "mails_up", 2: "mails_down", 3: "mails_rejected", 4: "control_bytes_up", 5: "control_bytes_down",
	6: "reconnects", 7: "chip_restarts", 8: "rtt_p50_ms", 9: "rtt_p95_ms", 10: "rtp_sent",
	11: "rtp_received", 12: "rtp_lost", 13: "chip_queue_hwm", 14: "p2_dropped",
	15: "stub_queue_drops", 16: "stub_csum_errors", 17: "stub_framing_errors", 18: "stub_seq_gaps", 19: "stub_resyncs",
}

// DecodeStats returns the counters of a STATS body (tag 1, repeatable,
// counter_id u16 | value u64). Unknown ids are kept by number.
func DecodeStats(b []byte) (map[uint16]uint64, error) {
	t, err := ParseTLV(b)
	if err != nil {
		return nil, err
	}
	out := map[uint16]uint64{}
	for _, v := range t.All(1) {
		if len(v) != 10 {
			return nil, fmt.Errorf("%w: STATS entry of %d bytes", ErrMalformed, len(v))
		}
		out[binary.BigEndian.Uint16(v)] = binary.BigEndian.Uint64(v[2:])
	}
	return out, nil
}

// EncodeStats builds a STATS body.
func EncodeStats(c map[uint16]uint64) []byte {
	w := new(Builder)
	for id, v := range c {
		e := binary.BigEndian.AppendUint16(nil, id)
		w.Bytes(1, binary.BigEndian.AppendUint64(e, v))
	}
	return w.Build()
}

// HeartbeatBody encodes the fixed 4-byte HEARTBEAT/HEARTBEAT_ECHO body.
func HeartbeatBody(t uint32) []byte { return binary.BigEndian.AppendUint32(nil, t) }

// DecodeHeartbeat parses a HEARTBEAT or HEARTBEAT_ECHO body.
func DecodeHeartbeat(b []byte) (uint32, error) {
	if len(b) != 4 {
		return 0, fmt.Errorf("%w: heartbeat body of %d bytes", ErrMalformed, len(b))
	}
	return binary.BigEndian.Uint32(b), nil
}

// FlowMsg is FLOW (§9.3).
type FlowMsg struct {
	QueueBytes uint16
	State      uint8
}

func DecodeFlow(b []byte) (FlowMsg, error) {
	if len(b) != 3 {
		return FlowMsg{}, fmt.Errorf("%w: FLOW body of %d bytes", ErrMalformed, len(b))
	}
	return FlowMsg{QueueBytes: binary.BigEndian.Uint16(b), State: b[2]}, nil
}

func (m FlowMsg) Encode() []byte {
	return append(binary.BigEndian.AppendUint16(nil, m.QueueBytes), m.State)
}

// RenewRequest/Response (§5.4).
func DecodeRenewRequest(b []byte) ([]byte, error) {
	t, err := ParseTLV(b)
	if err != nil {
		return nil, err
	}
	csr, err := need(t, 1, -1)
	if err == nil && len(csr) > 1024 {
		err = fmt.Errorf("%w: CSR longer than 1024 bytes", ErrMalformed)
	}
	return csr, err
}

func EncodeRenewResponse(status uint16, cert []byte) []byte {
	w := new(Builder).U16(1, status)
	if cert != nil {
		w.Bytes(2, cert)
	}
	return w.Build()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// Audio formats on the cell link (cell-protocol.md §8.2).
const (
	FormatL16_8k  uint8 = 1
	FormatL16_16k uint8 = 2
	FormatPCMA    uint8 = 3
	FormatPCMU    uint8 = 4
	FormatPCMA16k uint8 = 5
	FormatG722    uint8 = 6
	FormatG726_32 uint8 = 7 // reserved; unverified until a real G.726 call is measured
)

// SRTP profiles on the cell link (§8.1 MEDIA_OPEN tag 9).
const (
	SRTPNone        uint8 = 0
	SRTPAES128SHA80 uint8 = 1 // AES_CM_128_HMAC_SHA1_80: 16-octet key ‖ 14-octet salt
	SRTPAEAD128GCM  uint8 = 2 // AEAD_AES_128_GCM: 16-octet key ‖ 12-octet salt
)

// SRTPKeyLen is the master key ‖ salt length of a profile, 0 if unknown.
func SRTPKeyLen(profile uint8) int {
	switch profile {
	case SRTPAES128SHA80:
		return 30
	case SRTPAEAD128GCM:
		return 28
	}
	return 0
}

// MediaOpenMsg is MEDIA_OPEN (§8.1). FormatUp 0 means the same as Format.
type MediaOpenMsg struct {
	MediaID      uint32
	AudioChannel uint8
	Format       uint8 // downlink, and uplink unless FormatUp is set
	FormatUp     uint8 // tag 13, proposed (opendectcell#82)
	PtimeMs      uint8 // 0 = default 20
	PayloadType  uint8
	CtlAddr      netip.AddrPort
	SSRCUp       uint32
	SSRCDown     uint32
	SRTPProfile  uint8
	KeyUp        []byte
	KeyDown      []byte
	DSCP         uint8 // 0 = default 46
}

// Uplink is the format the cell sends.
func (m MediaOpenMsg) Uplink() uint8 {
	if m.FormatUp != 0 {
		return m.FormatUp
	}
	return m.Format
}

func (m MediaOpenMsg) Encode() []byte {
	w := new(Builder).U32(1, m.MediaID).U8(2, m.AudioChannel).U8(3, m.Format)
	if m.PtimeMs != 0 {
		w.U8(4, m.PtimeMs)
	}
	w.U8(5, m.PayloadType)
	a := m.CtlAddr.Addr().Unmap()
	fam := []byte{4}
	if a.Is6() {
		fam = []byte{6}
	}
	addr := append(append(fam, a.AsSlice()...), byte(m.CtlAddr.Port()>>8), byte(m.CtlAddr.Port()))
	w.Bytes(6, addr).U32(7, m.SSRCUp).U32(8, m.SSRCDown).U8(9, m.SRTPProfile)
	if m.SRTPProfile != SRTPNone {
		w.Bytes(10, m.KeyUp).Bytes(11, m.KeyDown)
	}
	if m.DSCP != 0 {
		w.U8(12, m.DSCP)
	}
	if m.FormatUp != 0 && m.FormatUp != m.Format {
		w.U8(13, m.FormatUp)
	}
	return w.Build()
}

func DecodeMediaOpen(b []byte) (MediaOpenMsg, error) {
	var m MediaOpenMsg
	t, err := ParseTLV(b)
	if err != nil {
		return m, err
	}
	if m.MediaID, err = t.U32(1); err != nil {
		return m, err
	}
	for tag, p := range map[uint8]*uint8{2: &m.AudioChannel, 3: &m.Format, 5: &m.PayloadType, 9: &m.SRTPProfile} {
		if *p, err = t.U8(tag); err != nil {
			return m, err
		}
	}
	for tag, p := range map[uint8]*uint8{4: &m.PtimeMs, 12: &m.DSCP, 13: &m.FormatUp} {
		if t.Has(tag) {
			if *p, err = t.U8(tag); err != nil {
				return m, err
			}
		}
	}
	a, ok := t.Get(6)
	switch {
	case !ok:
		return m, fmt.Errorf("%w: no ctl_addr", ErrMalformed)
	case len(a) == 7 && a[0] == 4:
		m.CtlAddr = netip.AddrPortFrom(netip.AddrFrom4([4]byte(a[1:5])), uint16(a[5])<<8|uint16(a[6]))
	case len(a) == 19 && a[0] == 6:
		m.CtlAddr = netip.AddrPortFrom(netip.AddrFrom16([16]byte(a[1:17])), uint16(a[17])<<8|uint16(a[18]))
	default:
		return m, fmt.Errorf("%w: bad ctl_addr", ErrMalformed)
	}
	if m.SSRCUp, err = t.U32(7); err != nil {
		return m, err
	}
	if m.SSRCDown, err = t.U32(8); err != nil {
		return m, err
	}
	if m.SRTPProfile != SRTPNone {
		n := SRTPKeyLen(m.SRTPProfile)
		if n == 0 {
			return m, fmt.Errorf("%w: unknown SRTP profile %d", ErrMalformed, m.SRTPProfile)
		}
		if m.KeyUp, err = t.Fixed(10, n); err != nil {
			return m, err
		}
		if m.KeyDown, err = t.Fixed(11, n); err != nil {
			return m, err
		}
	}
	return m, nil
}

// MailParamMax is the largest STUB mail parameter block (§7.1,
// libopendectcell's DECT_MAIL_PARAM_MAX).
const MailParamMax = 249

// MailUpMsg is MAIL_UP (§7.1): one STUB mail from the chip, verbatim.
// Param must never be logged for the primitives of §7.3.
type MailUpMsg struct {
	RxTime   uint32 // cell clock in µs, wraps; only differences mean anything
	Src, Dst uint8
	Prim     uint16
	Param    []byte
}

func DecodeMailUp(b []byte) (MailUpMsg, error) {
	if len(b) < 9 || len(b) != 9+int(b[8]) || b[8] > MailParamMax {
		return MailUpMsg{}, fmt.Errorf("%w: MAIL_UP body of %d bytes", ErrMalformed, len(b))
	}
	return MailUpMsg{RxTime: binary.BigEndian.Uint32(b), Src: b[4], Dst: b[5],
		Prim: binary.BigEndian.Uint16(b[6:]), Param: b[9:]}, nil
}

// Encode is the cell's side; the controller uses it in tests.
func (m MailUpMsg) Encode() []byte {
	b := binary.BigEndian.AppendUint32(nil, m.RxTime)
	return append(b, MailDownMsg{Src: m.Src, Dst: m.Dst, Prim: m.Prim, Param: m.Param}.Encode()...)
}

// MailDownMsg is MAIL_DOWN (§7.1): one STUB mail for the chip.
type MailDownMsg struct {
	Src, Dst uint8
	Prim     uint16
	Param    []byte
}

func DecodeMailDown(b []byte) (MailDownMsg, error) {
	if len(b) < 5 || len(b) != 5+int(b[4]) || b[4] > MailParamMax {
		return MailDownMsg{}, fmt.Errorf("%w: MAIL_DOWN body of %d bytes", ErrMalformed, len(b))
	}
	return MailDownMsg{Src: b[0], Dst: b[1], Prim: binary.BigEndian.Uint16(b[2:]), Param: b[5:]}, nil
}

// Encode panics on a parameter block over MailParamMax: the caller's bug.
func (m MailDownMsg) Encode() []byte {
	if len(m.Param) > MailParamMax {
		panic("wire: mail parameters over 249 bytes")
	}
	b := binary.BigEndian.AppendUint16([]byte{m.Src, m.Dst}, m.Prim)
	return append(append(b, byte(len(m.Param))), m.Param...)
}

// MAIL_REJECT reasons (§7.2).
const (
	RejectChipDown  uint8 = 1
	RejectQueueFull uint8 = 2
	RejectRefused   uint8 = 3
)

// MailRejectMsg is MAIL_REJECT (§7.2): the cell could not hand the
// MAIL_DOWN with this corr to the chip.
type MailRejectMsg struct {
	RefCorr uint16
	Reason  uint8
}

func DecodeMailReject(b []byte) (MailRejectMsg, error) {
	if len(b) != 3 {
		return MailRejectMsg{}, fmt.Errorf("%w: MAIL_REJECT body of %d bytes", ErrMalformed, len(b))
	}
	return MailRejectMsg{RefCorr: binary.BigEndian.Uint16(b), Reason: b[2]}, nil
}

func (m MailRejectMsg) Encode() []byte {
	return append(binary.BigEndian.AppendUint16(nil, m.RefCorr), m.Reason)
}
