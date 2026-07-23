package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SectionTypes are the HAProxy section kinds a snippet may declare. "raw" is
// appended verbatim at the end of the file for anything not listed.
var SectionTypes = []string{
	"global-extra", "defaults-extra", "listen", "resolvers", "peers", "userlist",
	"cache", "ring", "mailers", "http-errors", "program", "fcgi-app", "log-forward",
	"crt-store", "traces", "raw",
}

// SectionTakesArgument reports whether a section header carries a name.
func SectionTakesArgument(t string) bool {
	switch t {
	case "global-extra", "defaults-extra", "raw":
		return false
	}
	return true
}

func validSectionType(t string) bool {
	for _, s := range SectionTypes {
		if s == t {
			return true
		}
	}
	return false
}

const snippetColumns = `id, name, section_type, section_arg, body, enabled, order_index,
	created_at, updated_at`

func scanSnippet(row interface{ Scan(...any) error }) (*Snippet, error) {
	var (
		s                Snippet
		created, updated sql.NullString
		enabled          int
	)
	err := row.Scan(&s.ID, &s.Name, &s.SectionType, &s.SectionArg, &s.Body, &enabled,
		&s.OrderIndex, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.Enabled = enabled != 0
	s.CreatedAt = parseTime(created)
	s.UpdatedAt = parseTime(updated)
	return &s, nil
}

// ListSnippets returns every snippet in render order.
func (s *Store) ListSnippets(ctx context.Context) ([]*Snippet, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c,
		"SELECT "+snippetColumns+" FROM snippets ORDER BY order_index, section_type, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Snippet
	for rows.Next() {
		sn, err := scanSnippet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// GetSnippet loads one snippet.
func (s *Store) GetSnippet(ctx context.Context, id int64) (*Snippet, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanSnippet(s.db.QueryRowContext(c, "SELECT "+snippetColumns+" FROM snippets WHERE id = ?", id))
}

// SaveSnippet inserts or updates a snippet.
func (s *Store) SaveSnippet(ctx context.Context, sn *Snippet) (int64, error) {
	if err := ValidateName("snippet", sn.Name); err != nil {
		return 0, err
	}
	if !validSectionType(sn.SectionType) {
		return 0, fmt.Errorf("unknown section type %q", sn.SectionType)
	}
	if SectionTakesArgument(sn.SectionType) {
		if err := ValidateName("section", sn.SectionArg); err != nil {
			return 0, fmt.Errorf("%s sections need a name: %w", sn.SectionType, err)
		}
	}
	if strings.TrimSpace(sn.Body) == "" {
		return 0, errors.New("snippet body is empty")
	}

	c, cancel := ctxTimeout(ctx)
	defer cancel()
	args := []any{sn.Name, sn.SectionType, sn.SectionArg, sn.Body, boolToInt(sn.Enabled), sn.OrderIndex}

	if sn.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO snippets (name, section_type, section_arg, body, enabled, order_index)
			VALUES (?,?,?,?,?,?)`, args...)
		if err != nil {
			if isUniqueViolation(err) {
				return 0, fmt.Errorf("a %s snippet named %q already exists", sn.SectionType, sn.Name)
			}
			return 0, err
		}
		return res.LastInsertId()
	}

	args = append(args, sn.ID)
	_, err := s.db.ExecContext(c, `
		UPDATE snippets SET name=?, section_type=?, section_arg=?, body=?, enabled=?, order_index=?,
			updated_at=datetime('now')
		WHERE id = ?`, args...)
	if err != nil && isUniqueViolation(err) {
		return 0, fmt.Errorf("a %s snippet named %q already exists", sn.SectionType, sn.Name)
	}
	return sn.ID, err
}

// DeleteSnippet removes a snippet.
func (s *Store) DeleteSnippet(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM snippets WHERE id = ?", id)
	return err
}
