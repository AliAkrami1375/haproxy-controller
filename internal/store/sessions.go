package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// NewToken returns a URL-safe random token with 256 bits of entropy.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken derives the value stored in the database. Session cookies are
// bearer credentials, so only their hash is persisted.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession issues a session and returns the opaque cookie value that
// identifies it, formatted as "<id>.<token>".
func (s *Store) CreateSession(ctx context.Context, userID int64, ip, userAgent string, ttl time.Duration) (string, *Session, error) {
	id, err := NewToken()
	if err != nil {
		return "", nil, err
	}
	token, err := NewToken()
	if err != nil {
		return "", nil, err
	}
	csrf, err := NewToken()
	if err != nil {
		return "", nil, err
	}

	sess := &Session{
		ID:         id,
		UserID:     userID,
		TokenHash:  hashToken(token),
		CSRFToken:  csrf,
		IP:         ip,
		UserAgent:  truncate(userAgent, 255),
		CreatedAt:  time.Now().UTC(),
		LastSeenAt: time.Now().UTC(),
		ExpiresAt:  time.Now().UTC().Add(ttl),
	}

	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err = s.db.ExecContext(c, `
		INSERT INTO sessions (id, user_id, token_hash, csrf_token, ip, user_agent, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.UserID, sess.TokenHash, sess.CSRFToken, sess.IP, sess.UserAgent,
		formatTime(sess.ExpiresAt))
	if err != nil {
		return "", nil, err
	}
	return id + "." + token, sess, nil
}

// LookupSession resolves a cookie value to a live session and its user. It
// returns ErrNotFound for anything expired, unknown, or malformed.
func (s *Store) LookupSession(ctx context.Context, cookie string) (*Session, *User, error) {
	id, token, ok := splitCookie(cookie)
	if !ok {
		return nil, nil, ErrNotFound
	}

	c, cancel := ctxTimeout(ctx)
	defer cancel()

	var (
		sess                       Session
		created, lastSeen, expires sql.NullString
	)
	err := s.db.QueryRowContext(c, `
		SELECT id, user_id, token_hash, csrf_token, ip, user_agent, created_at, last_seen_at, expires_at
		FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &sess.CSRFToken, &sess.IP, &sess.UserAgent,
			&created, &lastSeen, &expires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	sess.CreatedAt = parseTime(created)
	sess.LastSeenAt = parseTime(lastSeen)
	sess.ExpiresAt = parseTime(expires)

	if subtleCompare(sess.TokenHash, hashToken(token)) != 1 {
		return nil, nil, ErrNotFound
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		_ = s.DeleteSession(ctx, sess.ID)
		return nil, nil, ErrNotFound
	}

	user, err := s.GetUser(ctx, sess.UserID)
	if err != nil {
		return nil, nil, err
	}
	if !user.IsActive {
		_ = s.DeleteSession(ctx, sess.ID)
		return nil, nil, ErrNotFound
	}
	return &sess, user, nil
}

// TouchSession refreshes last-seen and slides the expiry window forward.
func (s *Store) TouchSession(ctx context.Context, id string, ttl time.Duration) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, `
		UPDATE sessions SET last_seen_at = datetime('now'), expires_at = ? WHERE id = ?`,
		formatTime(time.Now().UTC().Add(ttl)), id)
	return err
}

// DeleteSession logs a single session out.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM sessions WHERE id = ?", id)
	return err
}

// DeleteUserSessions logs a user out everywhere, used after a password or
// role change.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM sessions WHERE user_id = ?", userID)
	return err
}

// PurgeExpiredSessions removes stale rows; called periodically.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	res, err := s.db.ExecContext(c, "DELETE FROM sessions WHERE expires_at < datetime('now')")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListUserSessions returns a user's active sessions, newest first.
func (s *Store) ListUserSessions(ctx context.Context, userID int64) ([]*Session, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, `
		SELECT id, user_id, ip, user_agent, created_at, last_seen_at, expires_at
		FROM sessions WHERE user_id = ? AND expires_at > datetime('now')
		ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Session
	for rows.Next() {
		var (
			sess                       Session
			created, lastSeen, expires sql.NullString
		)
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.IP, &sess.UserAgent,
			&created, &lastSeen, &expires); err != nil {
			return nil, err
		}
		sess.CreatedAt = parseTime(created)
		sess.LastSeenAt = parseTime(lastSeen)
		sess.ExpiresAt = parseTime(expires)
		out = append(out, &sess)
	}
	return out, rows.Err()
}

func splitCookie(cookie string) (id, token string, ok bool) {
	for i := 0; i < len(cookie); i++ {
		if cookie[i] == '.' {
			id, token = cookie[:i], cookie[i+1:]
			return id, token, id != "" && token != ""
		}
	}
	return "", "", false
}

// subtleCompare is a constant-time string comparison returning 1 on equality.
func subtleCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	if diff == 0 {
		return 1
	}
	return 0
}

// ConstantTimeEqual reports whether two strings are equal without leaking
// their contents through timing. Used for CSRF token comparison.
func ConstantTimeEqual(a, b string) bool { return subtleCompare(a, b) == 1 }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
