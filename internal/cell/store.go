// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cell is the cell manager (ROADMAP C2): enrollment, the mTLS
// control link, the handshake, heartbeats, resume and cell status
// (docs/cell-protocol.md). The STUB mail tunnel is not here yet; it waits
// for the libopendectcell API changes (opendectcell-controller#46).
package cell

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/pki"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

// Errors returned to callers; handlers map them to HTTP statuses.
var (
	ErrNotFound      = errors.New("cell: not found")
	ErrTokenRejected = errors.New("cell: enrollment token rejected")
	ErrRateLimited   = errors.New("cell: too many rejected enrollment attempts")
	ErrNotAccepted   = errors.New("cell: certificate not accepted")
	ErrRevoked       = errors.New("cell: cell is revoked")
	ErrBadState      = errors.New("cell: not allowed in the cell's current state")
)

// Enrollment limits (§5.1, §5.2).
const (
	DefaultTokenTTL   = 24 * time.Hour
	MaxTokenTTL       = 7 * 24 * time.Hour
	MinTokenTTL       = 5 * time.Minute
	enrollFailLimit   = 5
	enrollFailWindow  = 10 * time.Minute
	settingInstallKey = "installation_id"
)

// Store is the cell manager's database side.
type Store struct {
	Pool         *pgxpool.Pool
	Keyring      *secret.Keyring
	Installation uuid.UUID
	CA           *pki.CA
	// PublicAddr is the host:port cells reach the control port at; it goes
	// into enrollment strings.
	PublicAddr string
	Now        func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Open loads (or, on first start, creates) the installation id and the
// active cell CA.
func Open(ctx context.Context, pool *pgxpool.Pool, kr *secret.Keyring, publicAddr string) (*Store, error) {
	s := &Store{Pool: pool, Keyring: kr, PublicAddr: publicAddr}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "LOCK TABLE cell_ca IN EXCLUSIVE MODE"); err != nil {
			return err
		}
		inst, err := installationID(ctx, tx)
		if err != nil {
			return err
		}
		s.Installation = inst
		var id int32
		var certDER, keyEnc []byte
		err = tx.QueryRow(ctx, "SELECT id, cert_der, key_enc FROM cell_ca WHERE active").Scan(&id, &certDER, &keyEnc)
		if errors.Is(err, pgx.ErrNoRows) {
			s.CA, err = createCA(ctx, tx, kr, inst, s.now())
			return err
		}
		if err != nil {
			return err
		}
		keyDER, err := kr.Open(caLoc(id), keyEnc)
		if err != nil {
			return fmt.Errorf("cell: CA key: %w", err)
		}
		defer clear(keyDER)
		s.CA, err = pki.LoadCA(certDER, keyDER)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func caLoc(id int32) secret.Location {
	return secret.Location{Table: "cell_ca", Column: "key_enc", RowID: fmt.Sprint(id)}
}

func installationID(ctx context.Context, q db.Querier) (uuid.UUID, error) {
	var raw []byte
	err := q.QueryRow(ctx, "SELECT value FROM setting WHERE key = $1", settingInstallKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		id := uuid.New()
		v, _ := json.Marshal(id.String())
		_, err = q.Exec(ctx, "INSERT INTO setting (key, value) VALUES ($1, $2)", settingInstallKey, v)
		return id, err
	}
	if err != nil {
		return uuid.Nil, err
	}
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(str)
}

func createCA(ctx context.Context, tx pgx.Tx, kr *secret.Keyring, inst uuid.UUID, now time.Time) (*pki.CA, error) {
	ca, err := pki.NewCA(inst, now)
	if err != nil {
		return nil, err
	}
	var id int32
	if err := tx.QueryRow(ctx, "INSERT INTO cell_ca (cert_der, key_enc, active, not_after) VALUES ($1, '\\x', true, $2) RETURNING id",
		ca.Cert.Raw, ca.Cert.NotAfter).Scan(&id); err != nil {
		return nil, err
	}
	keyDER, err := ca.KeyDER()
	if err != nil {
		return nil, err
	}
	defer clear(keyDER)
	enc, err := kr.Seal(caLoc(id), keyDER)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "UPDATE cell_ca SET key_enc = $2 WHERE id = $1", id, enc); err != nil {
		return nil, err
	}
	return ca, audit.RecordAs(ctx, tx, audit.System, audit.Entry{Action: "cell_ca.create", ObjectType: "cell_ca",
		ObjectID: fmt.Sprint(id), Details: map[string]any{"ca_spki_sha256": ca.Pin(), "not_after": ca.Cert.NotAfter}})
}

// enrollment tokens

// Token is an enrollment token as listed (never with its secret).
type Token struct {
	ID          uuid.UUID
	Note        string
	CreatedBy   *uuid.UUID
	CreatedAt   time.Time
	ExpiresAt   time.Time
	AutoApprove bool
	UsedAt      *time.Time
	CellID      *uuid.UUID
}

// CreateToken makes a one-time enrollment token and returns it with its
// enrollment string, which is shown once (§5.1). Admins and users with
// may_add_cells may create tokens; only admins may set auto-approve.
func (s *Store) CreateToken(ctx context.Context, u *auth.User, note string, ttl time.Duration, autoApprove bool) (*Token, string, error) {
	if !auth.CanAddCells(u) || (autoApprove && !u.IsAdmin()) {
		return nil, "", auth.ErrForbidden
	}
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	if ttl < MinTokenTTL || ttl > MaxTokenTTL {
		return nil, "", fmt.Errorf("token lifetime must be between %v and %v", MinTokenTTL, MaxTokenTTL)
	}
	if len(note) > 128 {
		return nil, "", errors.New("note must be at most 128 characters")
	}
	raw := make([]byte, wire.TokenLen)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	t := &Token{Note: note, CreatedBy: &u.ID, AutoApprove: autoApprove}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO cell_enrollment_token (token_hash, created_by, note, auto_approve, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at, expires_at`,
			auth.HashToken(string(raw)), u.ID, note, autoApprove, s.now(), s.now().Add(ttl)).Scan(&t.ID, &t.CreatedAt, &t.ExpiresAt)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "cell_token.create", ObjectType: "cell_token", ObjectID: t.ID.String(),
			Details: map[string]any{"note": note, "auto_approve": autoApprove, "expires_at": t.ExpiresAt}})
	})
	if err != nil {
		return nil, "", err
	}
	es := pki.Enrollment{Addr: s.PublicAddr, Token: raw, Pin: s.CA.Pin()}.String()
	clear(raw)
	return t, es, nil
}

// The column list of cell_enrollment_token; it holds no token (G101 is a false positive).
const tokenCols = "id, note, created_by, created_at, expires_at, auto_approve, used_at, cell_id" //nolint:gosec // G101

func scanToken(r pgx.Row) (*Token, error) {
	var t Token
	err := r.Scan(&t.ID, &t.Note, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.AutoApprove, &t.UsedAt, &t.CellID)
	return &t, err
}

// ListTokens returns tokens newest first: all for admins, the caller's own
// for users with may_add_cells.
func (s *Store) ListTokens(ctx context.Context, u *auth.User) ([]*Token, error) {
	if !auth.CanAddCells(u) {
		return nil, auth.ErrForbidden
	}
	rows, err := s.Pool.Query(ctx, "SELECT "+tokenCols+" FROM cell_enrollment_token WHERE $1 OR created_by = $2 ORDER BY created_at DESC LIMIT 200",
		u.IsAdmin(), u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteToken removes a token. Its creator or an admin may; anyone else gets
// ErrNotFound.
func (s *Store) DeleteToken(ctx context.Context, u *auth.User, id uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		t, err := scanToken(tx.QueryRow(ctx, "SELECT "+tokenCols+" FROM cell_enrollment_token WHERE id = $1 FOR UPDATE", id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !auth.CanAccess(u, t.CreatedBy) || !auth.CanAddCells(u) {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, "DELETE FROM cell_enrollment_token WHERE id = $1", id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "cell_token.delete", ObjectType: "cell_token", ObjectID: id.String()})
	})
}

// EnrollResult is what a successful enrollment returns to the cell.
type EnrollResult struct {
	CellID uuid.UUID
	Cert   *x509.Certificate
	State  wire.CellStateValue
}

// Enroll checks and consumes a token, creates the cell and issues its
// certificate, all in one transaction (§5.2). Every attempt is audited with
// the source address, never the token. addr is used for rate limiting.
func (s *Store) Enroll(ctx context.Context, req wire.EnrollRequestMsg, addr string) (*EnrollResult, error) {
	ctx = audit.WithActor(ctx, audit.Actor{Name: "cell-enrollment", Addr: audit.ActorFrom(ctx).Addr})
	limited, err := s.enrollLimited(ctx, addr)
	if err != nil {
		return nil, err
	}
	if limited {
		_ = audit.Record(ctx, s.Pool, audit.Entry{Action: "cell.enroll_ratelimited", ObjectType: "cell"})
		return nil, ErrRateLimited
	}
	pub, err := pki.CheckCSR(req.CSR)
	if err != nil {
		_ = audit.Record(ctx, s.Pool, audit.Entry{Action: "cell.enroll_failed", ObjectType: "cell", Details: map[string]any{"reason": "bad csr"}})
		return nil, err
	}
	var res *EnrollResult
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var tokID uuid.UUID
		var auto bool
		var by *uuid.UUID
		var note string
		err := tx.QueryRow(ctx, `SELECT id, auto_approve, created_by, note FROM cell_enrollment_token
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2 FOR UPDATE`,
			auth.HashToken(string(req.Token)), s.now()).Scan(&tokID, &auto, &by, &note)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTokenRejected
		}
		if err != nil {
			return err
		}
		state := "pending"
		if auto {
			state = "active"
		}
		name, err := s.uniqueName(ctx, tx, firstNonEmpty(req.NameHint, note, "cell"))
		if err != nil {
			return err
		}
		var cellID uuid.UUID
		now := s.now()
		var approvedAt *time.Time
		if auto {
			approvedAt = &now
		}
		err = tx.QueryRow(ctx, `INSERT INTO cell (name, state, enrolled_by, enrolled_at, approved_at, software_version, location)
			VALUES ($1, $2, $3, $4, $5, $6, '') RETURNING id`, name, state, by, now, approvedAt, req.SWVersion).Scan(&cellID)
		if err != nil {
			return err
		}
		cert, err := s.CA.CellCert(cellID, pub, now)
		if err != nil {
			return err
		}
		if err := insertCert(ctx, tx, cellID, cert); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE cell_enrollment_token SET used_at = $2, cell_id = $3 WHERE id = $1", tokID, now, cellID); err != nil {
			return err
		}
		st, _ := wire.ParseCellState(state)
		res = &EnrollResult{CellID: cellID, Cert: cert, State: st}
		return audit.Record(ctx, tx, audit.Entry{Action: "cell.enroll", ObjectType: "cell", ObjectID: cellID.String(),
			Details: map[string]any{"name": name, "state": state, "token_id": tokID.String(), "hw_model": req.HWModel, "sw_version": req.SWVersion}})
	})
	if errors.Is(err, ErrTokenRejected) {
		if ferr := s.recordEnrollFailure(ctx, addr); ferr != nil {
			return nil, ferr
		}
		_ = audit.Record(ctx, s.Pool, audit.Entry{Action: "cell.enroll_failed", ObjectType: "cell", Details: map[string]any{"reason": "token rejected"}})
	}
	return res, err
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// uniqueName returns name, or name-2, name-3, … if it is taken.
func (s *Store) uniqueName(ctx context.Context, tx pgx.Tx, name string) (string, error) {
	if len(name) > 56 {
		name = strings.ToValidUTF8(name[:56], "")
	}
	for i := 1; i < 1000; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s-%d", name, i)
		}
		var taken bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM cell WHERE lower(name) = lower($1))", cand).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return cand, nil
		}
	}
	return "", errors.New("cell: no free name")
}

func insertCert(ctx context.Context, q db.Querier, cellID uuid.UUID, c *x509.Certificate) error {
	_, err := q.Exec(ctx, "INSERT INTO cell_certificate (fingerprint, cell_id, serial, not_before, not_after) VALUES ($1, $2, $3, $4, $5)",
		pki.Fingerprint(c.Raw), cellID, c.SerialNumber.Text(16), c.NotBefore, c.NotAfter)
	return err
}

func (s *Store) enrollLimited(ctx context.Context, addr string) (bool, error) {
	if addr == "" {
		return false, nil
	}
	var n int
	err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM login_failure WHERE key = $1 AND at > $2",
		"enroll:"+addr, s.now().Add(-enrollFailWindow)).Scan(&n)
	return n >= enrollFailLimit, err
}

func (s *Store) recordEnrollFailure(ctx context.Context, addr string) error {
	if addr == "" {
		return nil
	}
	_, err := s.Pool.Exec(ctx, "INSERT INTO login_failure (key, at) VALUES ($1, $2)", "enroll:"+addr, s.now())
	return err
}

// authentication of an established link

// Info is a cell's record as the link needs it.
type Info struct {
	ID    uuid.UUID
	Name  string
	State wire.CellStateValue
}

// Authenticate applies §4.2 steps 2–4 to a verified client certificate: the
// named cell exists, the certificate is in its current set, and the cell is
// not revoked. ErrRevoked is returned for a revoked cell so that the link can
// tell it to erase its credentials.
func (s *Store) Authenticate(ctx context.Context, cert *x509.Certificate) (*Info, error) {
	id, err := pki.CellIDFromCert(cert)
	if err != nil {
		return nil, ErrNotAccepted
	}
	var info Info
	var state string
	var current bool
	err = s.Pool.QueryRow(ctx, `SELECT c.id, c.name, c.state,
			EXISTS (SELECT 1 FROM cell_certificate k WHERE k.cell_id = c.id AND k.fingerprint = $2
			        AND k.revoked_at IS NULL AND k.not_after > $3)
		FROM cell c WHERE c.id = $1`, id, pki.Fingerprint(cert.Raw), s.now()).Scan(&info.ID, &info.Name, &state, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotAccepted
	}
	if err != nil {
		return nil, err
	}
	info.State, _ = wire.ParseCellState(state)
	if info.State == wire.StateRevoked {
		return &info, ErrRevoked
	}
	if !current {
		return &info, ErrNotAccepted
	}
	return &info, nil
}

// Renew issues a new certificate for an authenticated cell (§5.4) and adds
// it to the cell's current set.
func (s *Store) Renew(ctx context.Context, cellID uuid.UUID, csr []byte) (*x509.Certificate, error) {
	pub, err := pki.CheckCSR(csr)
	if err != nil {
		return nil, err
	}
	var cert *x509.Certificate
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		cert, err = s.CA.CellCert(cellID, pub, s.now())
		if err != nil {
			return err
		}
		if err := insertCert(ctx, tx, cellID, cert); err != nil {
			return err
		}
		return audit.RecordAs(ctx, tx, audit.Actor{Name: "cell:" + cellID.String()}, audit.Entry{Action: "cell.cert_renew",
			ObjectType: "cell", ObjectID: cellID.String(), Details: map[string]any{"serial": cert.SerialNumber.Text(16), "not_after": cert.NotAfter}})
	})
	return cert, err
}

// RetireOldCerts drops a cell's older certificates once it has connected
// with cert (§5.4: "as soon as the cell has connected with the new one").
// Older means issued before cert; a cell connecting with its previous
// certificate during a renewal must not retire the new one.
func (s *Store) RetireOldCerts(ctx context.Context, cellID uuid.UUID, cert *x509.Certificate) error {
	_, err := s.Pool.Exec(ctx, `UPDATE cell_certificate SET revoked_at = $3
		WHERE cell_id = $1 AND fingerprint <> $2 AND revoked_at IS NULL
		  AND created_at < (SELECT created_at FROM cell_certificate WHERE fingerprint = $2)`,
		cellID, pki.Fingerprint(cert.Raw), s.now())
	return err
}

// RecordHello stores what the cell announced (§6.1).
func (s *Store) RecordHello(ctx context.Context, cellID uuid.UUID, h wire.HelloMsg) error {
	_, err := s.Pool.Exec(ctx, `UPDATE cell SET software_version = $2, stub_version = nullif($3, ''),
		cap_sub_records = $4, cap_audio_nb = $5, cap_audio_wb = $6, last_seen_at = $7 WHERE id = $1`,
		cellID, h.SWVersion, h.StubVersion, int16(h.CapSubRecords), int16(h.CapAudioNB), int16(h.CapAudioWB), s.now())
	return err
}

// Seen updates last_seen_at.
func (s *Store) Seen(ctx context.Context, cellID uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, "UPDATE cell SET last_seen_at = $2 WHERE id = $1", cellID, s.now())
	return err
}

// SlotsUnknown marks every slot of a cell unknown: the chip's table is lost
// (radio parked or chip restarted, §7.4).
func (s *Store) SlotsUnknown(ctx context.Context, cellID uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, "UPDATE cell_slot SET state = 'unknown' WHERE cell_id = $1", cellID)
	return err
}

// cells for the UI and API

// Cell is a cell record.
type Cell struct {
	ID              uuid.UUID
	Name            string
	Location        string
	State           string
	EnrolledBy      *uuid.UUID
	EnrolledAt      *time.Time
	ApprovedAt      *time.Time
	SoftwareVersion *string
	StubVersion     *string
	CapSubRecords   *int16
	CapAudioNB      *int16
	CapAudioWB      *int16
	MaxCalls        int16
	AllowPlainRTP   bool
	LastSeenAt      *time.Time
	CertExpiresAt   *time.Time
	Profile
}

// Profile is what the owner says about a base station: where it is, what box
// it is, and which of that may leave this controller (a public map later).
// Every sharing switch starts off.
type Profile struct {
	BoxModel          string
	Latitude          *float64
	Longitude         *float64
	PositionPrecision string // exact, approx (about 1 km) or region (about 10 km)
	ShareName         bool
	SharePosition     bool
	ShareSpecs        bool
	ShareStats        bool
}

const cellCols = `c.id, c.name, c.location, c.state, c.enrolled_by, c.enrolled_at, c.approved_at, c.software_version,
	c.stub_version, c.cap_sub_records, c.cap_audio_nb, c.cap_audio_wb, c.max_calls, c.allow_plain_rtp, c.last_seen_at,
	(SELECT max(not_after) FROM cell_certificate k WHERE k.cell_id = c.id AND k.revoked_at IS NULL),
	c.box_model, c.latitude, c.longitude, c.position_precision, c.share_name, c.share_position, c.share_specs, c.share_stats`

func scanCell(r pgx.Row) (*Cell, error) {
	var c Cell
	err := r.Scan(&c.ID, &c.Name, &c.Location, &c.State, &c.EnrolledBy, &c.EnrolledAt, &c.ApprovedAt, &c.SoftwareVersion,
		&c.StubVersion, &c.CapSubRecords, &c.CapAudioNB, &c.CapAudioWB, &c.MaxCalls, &c.AllowPlainRTP, &c.LastSeenAt, &c.CertExpiresAt,
		&c.BoxModel, &c.Latitude, &c.Longitude, &c.PositionPrecision, &c.ShareName, &c.SharePosition, &c.ShareSpecs, &c.ShareStats)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// ListCells returns cells by name. Admins get every cell; users get active
// cells only, and the caller must show them only id, name and location
// (docs/api/openapi.yaml, listCells).
func (s *Store) ListCells(ctx context.Context, u *auth.User) ([]*Cell, error) {
	if u == nil {
		return nil, auth.ErrForbidden
	}
	rows, err := s.Pool.Query(ctx, "SELECT "+cellCols+" FROM cell c WHERE $1 OR c.state = 'active' ORDER BY lower(c.name)", u.IsAdmin())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Cell
	for rows.Next() {
		c, err := scanCell(rows)
		if err != nil {
			return nil, err
		}
		if !u.IsAdmin() {
			c = &Cell{ID: c.ID, Name: c.Name, Location: c.Location, State: c.State}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCell returns one cell; admins only.
func (s *Store) GetCell(ctx context.Context, u *auth.User, id uuid.UUID) (*Cell, error) {
	if auth.RequireAdmin(u) != nil {
		return nil, ErrNotFound
	}
	return scanCell(s.Pool.QueryRow(ctx, "SELECT "+cellCols+" FROM cell c WHERE c.id = $1", id))
}

// SetState approves (pending → active), disables or re-enables a cell.
// Revocation is Revoke. It returns the new state for the link to send.
func (s *Store) SetState(ctx context.Context, u *auth.User, id uuid.UUID, to string) (wire.CellStateValue, error) {
	if auth.RequireAdmin(u) != nil {
		return 0, ErrNotFound
	}
	if to != "active" && to != "disabled" {
		return 0, fmt.Errorf("%w: state must be active or disabled", ErrBadState)
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var from string
		err := tx.QueryRow(ctx, "SELECT state FROM cell WHERE id = $1 FOR UPDATE", id).Scan(&from)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if from == "revoked" {
			return ErrBadState
		}
		if from == to {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE cell SET state = $2,
			approved_at = CASE WHEN $2 = 'active' AND approved_at IS NULL THEN $3 ELSE approved_at END,
			approved_by = CASE WHEN $2 = 'active' AND approved_by IS NULL THEN $4 ELSE approved_by END
			WHERE id = $1`, id, to, s.now(), u.ID); err != nil {
			return err
		}
		action := "cell.disable"
		if to == "active" {
			action = "cell.enable"
			if from == "pending" {
				action = "cell.approve"
			}
		}
		return audit.Record(ctx, tx, audit.Entry{Action: action, ObjectType: "cell", ObjectID: id.String(), Details: map[string]any{"from": from}})
	})
	st, _ := wire.ParseCellState(to)
	return st, err
}

// Revoke makes a cell permanently unusable (§5.5): state revoked, every
// certificate revoked, its subscription slots freed.
func (s *Store) Revoke(ctx context.Context, u *auth.User, id uuid.UUID) error {
	if auth.RequireAdmin(u) != nil {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE cell SET state = 'revoked' WHERE id = $1 AND state <> 'revoked'", id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM cell WHERE id = $1)", id).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
			return nil // already revoked
		}
		if _, err := tx.Exec(ctx, "UPDATE cell_certificate SET revoked_at = $2 WHERE cell_id = $1 AND revoked_at IS NULL", id, s.now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM cell_slot WHERE cell_id = $1", id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "cell.revoke", ObjectType: "cell", ObjectID: id.String()})
	})
}

// CellUpdate holds the editable fields; nil leaves a field unchanged.
type CellUpdate struct {
	Name, Location *string
	MaxCalls       *int16
	AllowPlainRTP  *bool
}

// Update edits a cell's settings; admins only.
func (s *Store) Update(ctx context.Context, u *auth.User, id uuid.UUID, up CellUpdate) (*Cell, error) {
	if auth.RequireAdmin(u) != nil {
		return nil, ErrNotFound
	}
	if up.Name != nil && (strings.TrimSpace(*up.Name) == "" || len(*up.Name) > 64) {
		return nil, errors.New("name must be 1 to 64 characters")
	}
	if up.MaxCalls != nil && (*up.MaxCalls < 0 || *up.MaxCalls > 16) {
		return nil, errors.New("max calls must be 0 to 16")
	}
	var c *Cell
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE cell SET name = coalesce($2, name), location = coalesce($3, location),
			max_calls = coalesce($4, max_calls), allow_plain_rtp = coalesce($5, allow_plain_rtp) WHERE id = $1`,
			id, up.Name, up.Location, up.MaxCalls, up.AllowPlainRTP)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		d := map[string]any{}
		if up.Name != nil {
			d["name"] = *up.Name
		}
		if up.Location != nil {
			d["location"] = *up.Location
		}
		if up.MaxCalls != nil {
			d["max_calls"] = *up.MaxCalls
		}
		if up.AllowPlainRTP != nil {
			d["allow_plain_rtp"] = *up.AllowPlainRTP
		}
		if err := audit.Record(ctx, tx, audit.Entry{Action: "cell.update", ObjectType: "cell", ObjectID: id.String(), Details: d}); err != nil {
			return err
		}
		c, err = scanCell(tx.QueryRow(ctx, "SELECT "+cellCols+" FROM cell c WHERE c.id = $1", id))
		return err
	})
	return c, err
}

// ListOwned returns the cells u enrolled (admins: every cell), for the "my
// base stations" page.
func (s *Store) ListOwned(ctx context.Context, u *auth.User) ([]*Cell, error) {
	if u == nil {
		return nil, auth.ErrForbidden
	}
	rows, err := s.Pool.Query(ctx, "SELECT "+cellCols+" FROM cell c WHERE ($1 OR c.enrolled_by = $2) AND c.state <> 'revoked' ORDER BY lower(c.name)", u.IsAdmin(), u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Cell
	for rows.Next() {
		c, err := scanCell(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetOwned returns a cell u enrolled, or any cell for an admin; others get
// ErrNotFound, so a cell's existence is not revealed.
func (s *Store) GetOwned(ctx context.Context, u *auth.User, id uuid.UUID) (*Cell, error) {
	if u == nil {
		return nil, ErrNotFound
	}
	c, err := scanCell(s.Pool.QueryRow(ctx, "SELECT "+cellCols+" FROM cell c WHERE c.id = $1", id))
	if err != nil {
		return nil, err
	}
	if !u.IsAdmin() && (c.EnrolledBy == nil || *c.EnrolledBy != u.ID) {
		return nil, ErrNotFound
	}
	return c, nil
}

// ProfileUpdate is the owner's edit of a base station: its name and
// location text (also set by admins) and the Profile.
type ProfileUpdate struct {
	Name, Location string
	Profile
}

// UpdateProfile saves a base station's name, place and sharing choices. The
// owner or an admin may; the change is audited with the switches' new values
// but never with the coordinates.
func (s *Store) UpdateProfile(ctx context.Context, u *auth.User, id uuid.UUID, up ProfileUpdate) (*Cell, error) {
	name := strings.TrimSpace(up.Name)
	if name == "" || len(name) > 64 {
		return nil, errors.New("name must be 1 to 64 characters")
	}
	if len(up.Location) > 128 || len(up.BoxModel) > 64 {
		return nil, errors.New("location is at most 128 characters, the model at most 64")
	}
	if (up.Latitude == nil) != (up.Longitude == nil) {
		return nil, errors.New("give both latitude and longitude, or neither")
	}
	if up.Latitude != nil && (*up.Latitude < -90 || *up.Latitude > 90 || *up.Longitude < -180 || *up.Longitude > 180) {
		return nil, errors.New("latitude is -90 to 90, longitude -180 to 180")
	}
	switch up.PositionPrecision {
	case "exact", "approx", "region":
	default:
		return nil, errors.New("precision must be exact, approx or region")
	}
	if up.SharePosition && up.Latitude == nil {
		return nil, errors.New("set a position before sharing it")
	}
	var c *Cell
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var owner *uuid.UUID
		if err := tx.QueryRow(ctx, "SELECT enrolled_by FROM cell WHERE id = $1 AND state <> 'revoked' FOR UPDATE", id).Scan(&owner); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !u.IsAdmin() && (owner == nil || *owner != u.ID) {
			return ErrNotFound
		}
		_, err := tx.Exec(ctx, `UPDATE cell SET name = $2, location = $3, box_model = $4, latitude = $5, longitude = $6,
			position_precision = $7, share_name = $8, share_position = $9, share_specs = $10, share_stats = $11 WHERE id = $1`,
			id, name, up.Location, strings.TrimSpace(up.BoxModel), up.Latitude, up.Longitude, up.PositionPrecision,
			up.ShareName, up.SharePosition, up.ShareSpecs, up.ShareStats)
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Entry{Action: "cell.profile", ObjectType: "cell", ObjectID: id.String(), Details: map[string]any{
			"name": name, "has_position": up.Latitude != nil, "precision": up.PositionPrecision,
			"share_name": up.ShareName, "share_position": up.SharePosition, "share_specs": up.ShareSpecs, "share_stats": up.ShareStats}}); err != nil {
			return err
		}
		c, err = scanCell(tx.QueryRow(ctx, "SELECT "+cellCols+" FROM cell c WHERE c.id = $1", id))
		return err
	})
	return c, err
}

// PublicStats are the counts a station shares when its owner switched
// statistics on: numbers only, no times of day, no numbers dialled, no
// handsets.
type PublicStats struct {
	ConnectedHandsets int   `json:"handsets"`
	UptimeSeconds     int64 `json:"uptime_s"`
	Calls7d           int   `json:"calls_7d"`
	TalkSeconds7d     int64 `json:"talk_s_7d"`
}

// PublicStation is what a base station shares with the world, after its
// owner's switches: a field the owner did not share is not in the struct at
// all (nil or empty), whatever else is stored. It never holds an address, a
// handset or a number.
type PublicStation struct {
	ID        uuid.UUID    `json:"id"`
	Name      string       `json:"name,omitempty"`
	Latitude  *float64     `json:"lat,omitempty"`
	Longitude *float64     `json:"lon,omitempty"`
	BoxModel  string       `json:"model,omitempty"`
	Stats     *PublicStats `json:"stats,omitempty"`
}

// BlurPosition rounds a position to the precision the owner chose: approx
// keeps two decimals (about 1 km), region one (about 10 km).
func BlurPosition(lat, lon float64, precision string) (float64, float64) {
	switch precision {
	case "approx":
		return math.Round(lat*100) / 100, math.Round(lon*100) / 100
	case "region":
		return math.Round(lat*10) / 10, math.Round(lon*10) / 10
	}
	return lat, lon
}

// Public returns the shared view of an active cell; st is only used when the
// owner shares statistics. The id is a random UUID
// and always present, so a map can tell stations apart even when the owner
// shares no name.
func (c *Cell) Public(st PublicStats) PublicStation {
	p := PublicStation{ID: c.ID}
	if c.ShareStats {
		p.Stats = &st
	}
	if c.ShareName {
		p.Name = c.Name
	}
	if c.SharePosition && c.Latitude != nil && c.Longitude != nil {
		lat, lon := BlurPosition(*c.Latitude, *c.Longitude, c.PositionPrecision)
		p.Latitude, p.Longitude = &lat, &lon
	}
	if c.ShareSpecs {
		p.BoxModel = c.BoxModel
	}
	return p
}
