// Package store is the data access layer over the controller's SQLite database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ebdaa/haproxy-controller/internal/db"
)

// ErrNotFound is returned when a lookup by id or name matches no row.
var ErrNotFound = errors.New("not found")

// Store provides access to every entity the controller manages.
type Store struct {
	db *db.DB
}

// New wraps an open database.
func New(database *db.DB) *Store { return &Store{db: database} }

// DB exposes the underlying handle for callers that need transactions.
func (s *Store) DB() *db.DB { return s.db }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// sqliteTime is the layout SQLite's datetime('now') produces.
const sqliteTime = "2006-01-02 15:04:05"

// parseTime converts a SQLite timestamp string into a time.Time. Unparseable
// or empty values yield the zero time rather than an error, since timestamps
// are display metadata and must never fail a page render.
func parseTime(v sql.NullString) time.Time {
	if !v.Valid || v.String == "" {
		return time.Time{}
	}
	for _, layout := range []string{sqliteTime, time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, v.String); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func parseTimePtr(v sql.NullString) *time.Time {
	t := parseTime(v)
	if t.IsZero() {
		return nil
	}
	return &t
}

func formatTime(t time.Time) string { return t.UTC().Format(sqliteTime) }

func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// nameRe constrains identifiers that end up as HAProxy section names. HAProxy
// itself is permissive here, but restricting the character set removes any
// chance of a name breaking out of its line in the generated config.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidateName checks a proxy/section identifier.
func ValidateName(kind, name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%s name %q is invalid: use 1-63 characters from A-Z a-z 0-9 . _ - and start with a letter or digit", kind, name)
	}
	return nil
}

// hostRe constrains domain names accepted by the routing editor. A leading
// "*." is allowed so wildcard routes can be entered the way operators write
// them; the renderer strips it when building the ACL.
var hostRe = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// ValidateHostname checks a routing hostname, allowing a leading "*." wildcard.
func ValidateHostname(host string) error {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || len(host) > 253 {
		return fmt.Errorf("hostname must be between 1 and 253 characters")
	}
	if !hostRe.MatchString(host) {
		return fmt.Errorf("hostname %q is not a valid domain name", host)
	}
	return nil
}

// SplitLines returns the non-empty, trimmed lines of a multi-line field.
func SplitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ctx returns a request-scoped context with a sane timeout for DB work.
func ctxTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, 15*time.Second)
}
