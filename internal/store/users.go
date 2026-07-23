package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is deliberately above the library default to slow offline
// cracking of a stolen database.
const bcryptCost = 12

// MaxFailedLogins is how many consecutive bad passwords lock an account.
const MaxFailedLogins = 5

// LockoutDuration is how long an account stays locked after too many failures.
const LockoutDuration = 15 * time.Minute

const userColumns = `id, username, password_hash, full_name, email, role, is_active,
	must_change_pw, failed_attempts, locked_until, last_login_at, last_login_ip,
	created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var (
		u                                       User
		locked, lastLogin, createdAt, updatedAt sql.NullString
		isActive, mustChange                    int
	)
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.FullName, &u.Email, &u.Role,
		&isActive, &mustChange, &u.FailedAttempts, &locked, &lastLogin, &u.LastLoginIP,
		&createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.IsActive = isActive != 0
	u.MustChangePw = mustChange != 0
	u.LockedUntil = parseTimePtr(locked)
	u.LastLoginAt = parseTimePtr(lastLogin)
	u.CreatedAt = parseTime(createdAt)
	u.UpdatedAt = parseTime(updatedAt)
	return &u, nil
}

// GetUser looks a user up by id.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanUser(s.db.QueryRowContext(c, "SELECT "+userColumns+" FROM users WHERE id = ?", id))
}

// GetUserByUsername looks a user up by name, case-insensitively.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanUser(s.db.QueryRowContext(c,
		"SELECT "+userColumns+" FROM users WHERE username = ? COLLATE NOCASE", strings.TrimSpace(username)))
}

// ListUsers returns all accounts ordered by username.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, "SELECT "+userColumns+" FROM users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of accounts.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	var n int
	err := s.db.QueryRowContext(c, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

// CountAdmins returns the number of active administrators, used to refuse
// removing or demoting the last one.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	var n int
	err := s.db.QueryRowContext(c,
		"SELECT COUNT(*) FROM users WHERE role = ? AND is_active = 1", RoleAdmin).Scan(&n)
	return n, err
}

// ValidateUsername enforces the account name format.
func ValidateUsername(name string) error {
	name = strings.TrimSpace(name)
	if len(name) < 3 || len(name) > 32 {
		return errors.New("username must be between 3 and 32 characters")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return errors.New("username may contain only letters, digits, dot, underscore and hyphen")
		}
	}
	return nil
}

// ValidatePassword enforces the password policy.
func ValidatePassword(pw string) error {
	if len(pw) < 10 {
		return errors.New("password must be at least 10 characters")
	}
	if len(pw) > 200 {
		return errors.New("password must be at most 200 characters")
	}
	var hasLetter, hasDigit bool
	for _, r := range pw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return errors.New("password must contain at least one letter and one digit")
	}
	return nil
}

// ValidateRole checks that a role string is one the controller knows.
func ValidateRole(role string) error {
	switch role {
	case RoleAdmin, RoleOperator, RoleViewer:
		return nil
	}
	return fmt.Errorf("unknown role %q", role)
}

// HashPassword produces a bcrypt hash at the controller's cost.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

// CreateUser inserts a new account after validating every field.
func (s *Store) CreateUser(ctx context.Context, u *User, password string) (int64, error) {
	if err := ValidateUsername(u.Username); err != nil {
		return 0, err
	}
	if err := ValidatePassword(password); err != nil {
		return 0, err
	}
	if err := ValidateRole(u.Role); err != nil {
		return 0, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return 0, err
	}

	c, cancel := ctxTimeout(ctx)
	defer cancel()
	res, err := s.db.ExecContext(c, `
		INSERT INTO users (username, password_hash, full_name, email, role, is_active, must_change_pw)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(u.Username), hash, u.FullName, u.Email, u.Role,
		boolToInt(u.IsActive), boolToInt(u.MustChangePw))
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("a user named %q already exists", u.Username)
		}
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateUser saves profile fields. The password is handled separately.
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	if err := ValidateRole(u.Role); err != nil {
		return err
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, `
		UPDATE users SET full_name = ?, email = ?, role = ?, is_active = ?,
		                 must_change_pw = ?, updated_at = datetime('now')
		WHERE id = ?`,
		u.FullName, u.Email, u.Role, boolToInt(u.IsActive), boolToInt(u.MustChangePw), u.ID)
	return err
}

// SetPassword replaces a user's password, clears any lockout, and drops the
// forced-change flag.
func (s *Store) SetPassword(ctx context.Context, userID int64, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err = s.db.ExecContext(c, `
		UPDATE users SET password_hash = ?, must_change_pw = 0, failed_attempts = 0,
		                 locked_until = NULL, updated_at = datetime('now')
		WHERE id = ?`, hash, userID)
	return err
}

// DeleteUser removes an account and, by cascade, its sessions.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM users WHERE id = ?", id)
	return err
}

// VerifyPassword compares a plaintext password against the stored hash in
// constant time.
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// IsLocked reports whether the account is currently locked out.
func (u *User) IsLocked() bool {
	return u.LockedUntil != nil && u.LockedUntil.After(time.Now().UTC())
}

// RegisterFailedLogin increments the failure counter and locks the account
// once it crosses the threshold.
func (s *Store) RegisterFailedLogin(ctx context.Context, userID int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var attempts int
	if err := tx.QueryRowContext(c, "SELECT failed_attempts FROM users WHERE id = ?", userID).Scan(&attempts); err != nil {
		return err
	}
	attempts++

	if attempts >= MaxFailedLogins {
		until := formatTime(time.Now().UTC().Add(LockoutDuration))
		_, err = tx.ExecContext(c,
			"UPDATE users SET failed_attempts = ?, locked_until = ? WHERE id = ?", attempts, until, userID)
	} else {
		_, err = tx.ExecContext(c,
			"UPDATE users SET failed_attempts = ? WHERE id = ?", attempts, userID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RegisterSuccessfulLogin clears the failure state and records login metadata.
func (s *Store) RegisterSuccessfulLogin(ctx context.Context, userID int64, ip string) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, `
		UPDATE users SET failed_attempts = 0, locked_until = NULL,
		                 last_login_at = datetime('now'), last_login_ip = ?
		WHERE id = ?`, ip, userID)
	return err
}

// UnlockUser clears a lockout administratively.
func (s *Store) UnlockUser(ctx context.Context, userID int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c,
		"UPDATE users SET failed_attempts = 0, locked_until = NULL WHERE id = ?", userID)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
