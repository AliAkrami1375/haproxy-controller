package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
)

// Version statuses.
const (
	VersionApplied    = "applied"
	VersionFailed     = "failed"
	VersionRolledBack = "rolled_back"
)

// Checksum returns the SHA-256 of a rendered configuration.
func Checksum(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// AddVersion records a rendered configuration and returns its id.
func (s *Store) AddVersion(ctx context.Context, v *ConfigVersion) (int64, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	res, err := s.db.ExecContext(c, `
		INSERT INTO config_versions (content, checksum, status, comment, created_by, error)
		VALUES (?, ?, ?, ?, ?, ?)`,
		v.Content, Checksum(v.Content), v.Status, v.Comment, v.CreatedBy, truncate(v.Error, 4000))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const versionColumns = `id, content, checksum, status, comment, created_by, error, created_at`

func scanVersion(row interface{ Scan(...any) error }) (*ConfigVersion, error) {
	var (
		v       ConfigVersion
		created sql.NullString
	)
	err := row.Scan(&v.ID, &v.Content, &v.Checksum, &v.Status, &v.Comment, &v.CreatedBy, &v.Error, &created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	v.CreatedAt = parseTime(created)
	return &v, nil
}

// ListVersions returns configuration versions newest first. Content is omitted
// to keep the listing light.
func (s *Store) ListVersions(ctx context.Context, limit int) ([]*ConfigVersion, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, `
		SELECT id, '', checksum, status, comment, created_by, error, created_at
		FROM config_versions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*ConfigVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersion loads one version including its content.
func (s *Store) GetVersion(ctx context.Context, id int64) (*ConfigVersion, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanVersion(s.db.QueryRowContext(c, "SELECT "+versionColumns+" FROM config_versions WHERE id = ?", id))
}

// LatestApplied returns the most recent successfully applied version, used as
// the rollback target.
func (s *Store) LatestApplied(ctx context.Context) (*ConfigVersion, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanVersion(s.db.QueryRowContext(c,
		"SELECT "+versionColumns+" FROM config_versions WHERE status = ? ORDER BY id DESC LIMIT 1",
		VersionApplied))
}

// MarkVersion updates a version's status and error text.
func (s *Store) MarkVersion(ctx context.Context, id int64, status, errText string) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c,
		"UPDATE config_versions SET status = ?, error = ? WHERE id = ?", status, truncate(errText, 4000), id)
	return err
}

// TrimVersions keeps the newest rows and deletes the rest.
func (s *Store) TrimVersions(ctx context.Context, keep int) error {
	if keep < 5 {
		keep = 5
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, `
		DELETE FROM config_versions WHERE id NOT IN (
			SELECT id FROM config_versions ORDER BY id DESC LIMIT ?
		)`, keep)
	return err
}
