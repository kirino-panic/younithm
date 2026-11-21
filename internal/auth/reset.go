package auth

import (
	"context"
	"database/sql"
	"errors"
)

// ErrNoAdmin is returned by ResetAdmin when setup has never been completed.
var ErrNoAdmin = errors.New("auth: no admin account exists; complete first-run setup instead")

// ResetAdmin is the operator recovery path (run from the CLI against the
// local DB). It sets a new password for the admin, revokes every session,
// and clears login lockouts. It returns the username that was reset.
func (s *Service) ResetAdmin(ctx context.Context, newPassword string) (string, error) {
	if err := validatePassword(newPassword); err != nil {
		return "", err
	}
	var (
		id   int64
		name string
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, username FROM admin_user ORDER BY id LIMIT 1`).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoAdmin
	}
	if err != nil {
		return "", err
	}
	hash, err := s.hash(newPassword)
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE admin_user SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, ts(s.now()), id); err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_session WHERE user_id = ?`, id); err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_throttle`); err != nil {
		return "", err
	}
	return name, nil
}
