// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrForbidden is returned by the checks below. Handlers map it to 404 for
// objects (so their existence isn't revealed) and 403 for actions.
var ErrForbidden = errors.New("auth: forbidden")

// CanAccess is the ownership rule for owned objects (handsets, SIP
// accounts, call log entries): admins may access everything; a user only
// objects they own. owner nil means a shared, admin-managed object.
func CanAccess(u *User, owner *uuid.UUID) bool {
	if u == nil || u.Disabled {
		return false
	}
	if u.IsAdmin() {
		return true
	}
	return owner != nil && *owner == u.ID
}

// RequireAccess returns ErrForbidden unless CanAccess.
func RequireAccess(u *User, owner *uuid.UUID) error {
	if !CanAccess(u, owner) {
		return ErrForbidden
	}
	return nil
}

// RequireAdmin returns ErrForbidden unless u is an admin.
func RequireAdmin(u *User) error {
	if u == nil || u.Disabled || !u.IsAdmin() {
		return ErrForbidden
	}
	return nil
}

// CanAddCells reports whether u may create cell enrollment tokens: admins,
// and users with the may_add_cells permission (architecture.md §2).
func CanAddCells(u *User) bool {
	return u != nil && !u.Disabled && (u.IsAdmin() || u.MayAddCells)
}

type userKey struct{}

// WithUser returns a context carrying the authenticated user.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// UserFrom returns the authenticated user, or nil.
func UserFrom(ctx context.Context) *User {
	u, _ := ctx.Value(userKey{}).(*User)
	return u
}
