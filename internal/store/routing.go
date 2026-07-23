package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// MatchTypes are the host matching strategies the routing editor offers.
var MatchTypes = []string{"exact", "subdomain", "wildcard", "regex"}

func validMatchType(t string) bool {
	for _, m := range MatchTypes {
		if m == t {
			return true
		}
	}
	return false
}

const domainColumns = `id, hostname, match_type, path_prefix, frontend_id, backend_id,
	redirect_to, redirect_code, force_https, enabled, order_index, created_at`

func scanDomain(row interface{ Scan(...any) error }) (*Domain, error) {
	var (
		d                 Domain
		backendID         sql.NullInt64
		created           sql.NullString
		forceHTTPS, enabl int
	)
	err := row.Scan(&d.ID, &d.Hostname, &d.MatchType, &d.PathPrefix, &d.FrontendID, &backendID,
		&d.RedirectTo, &d.RedirectCode, &forceHTTPS, &enabl, &d.OrderIndex, &created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if backendID.Valid {
		id := backendID.Int64
		d.BackendID = &id
	}
	d.ForceHTTPS = forceHTTPS != 0
	d.Enabled = enabl != 0
	d.CreatedAt = parseTime(created)
	return &d, nil
}

// ListDomains returns every routing entry with its frontend and backend names
// resolved for display.
func (s *Store) ListDomains(ctx context.Context) ([]*Domain, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, `
		SELECT d.id, d.hostname, d.match_type, d.path_prefix, d.frontend_id, d.backend_id,
		       d.redirect_to, d.redirect_code, d.force_https, d.enabled, d.order_index, d.created_at,
		       f.name, COALESCE(b.name, '')
		FROM domains d
		JOIN frontends f ON f.id = d.frontend_id
		LEFT JOIN backends b ON b.id = d.backend_id
		ORDER BY d.order_index, d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Domain
	for rows.Next() {
		var (
			d                 Domain
			backendID         sql.NullInt64
			created           sql.NullString
			forceHTTPS, enabl int
		)
		if err := rows.Scan(&d.ID, &d.Hostname, &d.MatchType, &d.PathPrefix, &d.FrontendID, &backendID,
			&d.RedirectTo, &d.RedirectCode, &forceHTTPS, &enabl, &d.OrderIndex, &created,
			&d.FrontendName, &d.BackendName); err != nil {
			return nil, err
		}
		if backendID.Valid {
			id := backendID.Int64
			d.BackendID = &id
		}
		d.ForceHTTPS = forceHTTPS != 0
		d.Enabled = enabl != 0
		d.CreatedAt = parseTime(created)
		out = append(out, &d)
	}
	return out, rows.Err()
}

// ListDomainsByFrontend returns the routing entries attached to one frontend.
func (s *Store) ListDomainsByFrontend(ctx context.Context, frontendID int64) ([]Domain, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c,
		"SELECT "+domainColumns+" FROM domains WHERE frontend_id = ? ORDER BY order_index, hostname", frontendID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// GetDomain loads one routing entry.
func (s *Store) GetDomain(ctx context.Context, id int64) (*Domain, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanDomain(s.db.QueryRowContext(c, "SELECT "+domainColumns+" FROM domains WHERE id = ?", id))
}

// SaveDomain inserts or updates a routing entry. A route must either point at
// a backend or specify a redirect target.
func (s *Store) SaveDomain(ctx context.Context, d *Domain) (int64, error) {
	d.Hostname = strings.ToLower(strings.TrimSpace(d.Hostname))
	if d.MatchType == "regex" {
		if d.Hostname == "" {
			return 0, errors.New("a regular expression is required")
		}
	} else if err := ValidateHostname(d.Hostname); err != nil {
		return 0, err
	}
	if !validMatchType(d.MatchType) {
		return 0, fmt.Errorf("unknown match type %q", d.MatchType)
	}
	if d.BackendID == nil && strings.TrimSpace(d.RedirectTo) == "" {
		return 0, errors.New("choose a backend or enter a redirect target")
	}
	if d.PathPrefix != "" && !strings.HasPrefix(d.PathPrefix, "/") {
		return 0, errors.New("path prefix must start with /")
	}
	switch d.RedirectCode {
	case 0, 301, 302, 303, 307, 308:
	default:
		return 0, fmt.Errorf("redirect code %d is not supported", d.RedirectCode)
	}

	c, cancel := ctxTimeout(ctx)
	defer cancel()
	args := []any{d.Hostname, d.MatchType, d.PathPrefix, d.FrontendID, nullInt64(d.BackendID),
		d.RedirectTo, d.RedirectCode, boolToInt(d.ForceHTTPS), boolToInt(d.Enabled), d.OrderIndex}

	if d.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO domains (hostname, match_type, path_prefix, frontend_id, backend_id,
				redirect_to, redirect_code, force_https, enabled, order_index)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, args...)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}

	args = append(args, d.ID)
	_, err := s.db.ExecContext(c, `
		UPDATE domains SET hostname=?, match_type=?, path_prefix=?, frontend_id=?, backend_id=?,
			redirect_to=?, redirect_code=?, force_https=?, enabled=?, order_index=?
		WHERE id = ?`, args...)
	return d.ID, err
}

// DeleteDomain removes a routing entry.
func (s *Store) DeleteDomain(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM domains WHERE id = ?", id)
	return err
}

// CountDomains returns the number of routing entries.
func (s *Store) CountDomains(ctx context.Context) (int, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	var n int
	err := s.db.QueryRowContext(c, "SELECT COUNT(*) FROM domains").Scan(&n)
	return n, err
}

// -------------------------------------------------------------------- ACLs

// ListACLs returns the ACLs attached to a frontend or backend.
func (s *Store) ListACLs(ctx context.Context, scope string, ownerID int64) ([]ACL, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, `
		SELECT id, scope, owner_id, name, expression, enabled, order_index
		FROM acls WHERE scope = ? AND owner_id = ? ORDER BY order_index, id`, scope, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ACL
	for rows.Next() {
		var a ACL
		var enabled int
		if err := rows.Scan(&a.ID, &a.Scope, &a.OwnerID, &a.Name, &a.Expression, &enabled, &a.OrderIndex); err != nil {
			return nil, err
		}
		a.Enabled = enabled != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetACL loads one ACL.
func (s *Store) GetACL(ctx context.Context, id int64) (*ACL, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	var a ACL
	var enabled int
	err := s.db.QueryRowContext(c, `
		SELECT id, scope, owner_id, name, expression, enabled, order_index FROM acls WHERE id = ?`, id).
		Scan(&a.ID, &a.Scope, &a.OwnerID, &a.Name, &a.Expression, &enabled, &a.OrderIndex)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.Enabled = enabled != 0
	return &a, nil
}

// SaveACL inserts or updates an ACL.
func (s *Store) SaveACL(ctx context.Context, a *ACL) (int64, error) {
	if err := ValidateName("ACL", a.Name); err != nil {
		return 0, err
	}
	if strings.TrimSpace(a.Expression) == "" {
		return 0, errors.New("ACL expression is required")
	}
	if err := checkSingleLine("ACL expression", a.Expression); err != nil {
		return 0, err
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	if a.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO acls (scope, owner_id, name, expression, enabled, order_index)
			VALUES (?,?,?,?,?,?)`,
			a.Scope, a.OwnerID, a.Name, a.Expression, boolToInt(a.Enabled), a.OrderIndex)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	_, err := s.db.ExecContext(c, `
		UPDATE acls SET name=?, expression=?, enabled=?, order_index=? WHERE id = ?`,
		a.Name, a.Expression, boolToInt(a.Enabled), a.OrderIndex, a.ID)
	return a.ID, err
}

// DeleteACL removes an ACL.
func (s *Store) DeleteACL(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM acls WHERE id = ?", id)
	return err
}

// ------------------------------------------------------------------- rules

// AllowedDirectives is the set of ordered directives the rule editor accepts.
// Restricting it keeps a rule from redefining unrelated parts of a section.
var AllowedDirectives = []string{
	"http-request", "http-response", "http-after-response",
	"tcp-request connection", "tcp-request content", "tcp-request session",
	"tcp-response content", "redirect", "use_backend", "use-server",
	"filter", "compression", "capture request header", "capture response header",
	"stick", "stick-table", "monitor-uri", "monitor fail", "unique-id-format",
	"log-format", "http-check", "tcp-check", "option", "no option", "acl", "declare capture",
	"errorfile", "errorfiles", "rate-limit sessions", "email-alert",
}

func validDirective(d string) bool {
	for _, a := range AllowedDirectives {
		if a == d {
			return true
		}
	}
	return false
}

// ListRules returns the ordered rules attached to a frontend or backend.
func (s *Store) ListRules(ctx context.Context, scope string, ownerID int64) ([]Rule, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, `
		SELECT id, scope, owner_id, directive, argument, condition, enabled, order_index
		FROM rules WHERE scope = ? AND owner_id = ? ORDER BY order_index, id`, scope, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Rule
	for rows.Next() {
		var r Rule
		var enabled int
		if err := rows.Scan(&r.ID, &r.Scope, &r.OwnerID, &r.Directive, &r.Argument,
			&r.Condition, &enabled, &r.OrderIndex); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRule loads one rule.
func (s *Store) GetRule(ctx context.Context, id int64) (*Rule, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	var r Rule
	var enabled int
	err := s.db.QueryRowContext(c, `
		SELECT id, scope, owner_id, directive, argument, condition, enabled, order_index
		FROM rules WHERE id = ?`, id).
		Scan(&r.ID, &r.Scope, &r.OwnerID, &r.Directive, &r.Argument, &r.Condition, &enabled, &r.OrderIndex)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.Enabled = enabled != 0
	return &r, nil
}

// SaveRule inserts or updates a rule.
func (s *Store) SaveRule(ctx context.Context, r *Rule) (int64, error) {
	if !validDirective(r.Directive) {
		return 0, fmt.Errorf("directive %q is not allowed here; use a raw snippet for it", r.Directive)
	}
	if err := checkSingleLine("rule argument", r.Argument); err != nil {
		return 0, err
	}
	if err := checkSingleLine("rule condition", r.Condition); err != nil {
		return 0, err
	}
	cond := strings.TrimSpace(r.Condition)
	if cond != "" && !strings.HasPrefix(cond, "if ") && !strings.HasPrefix(cond, "unless ") {
		return 0, errors.New(`condition must start with "if " or "unless "`)
	}

	c, cancel := ctxTimeout(ctx)
	defer cancel()
	if r.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO rules (scope, owner_id, directive, argument, condition, enabled, order_index)
			VALUES (?,?,?,?,?,?,?)`,
			r.Scope, r.OwnerID, r.Directive, r.Argument, cond, boolToInt(r.Enabled), r.OrderIndex)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	_, err := s.db.ExecContext(c, `
		UPDATE rules SET directive=?, argument=?, condition=?, enabled=?, order_index=? WHERE id = ?`,
		r.Directive, r.Argument, cond, boolToInt(r.Enabled), r.OrderIndex, r.ID)
	return r.ID, err
}

// DeleteRule removes a rule.
func (s *Store) DeleteRule(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM rules WHERE id = ?", id)
	return err
}

// MoveRule shifts a rule up or down within its owner, renumbering as needed.
func (s *Store) MoveRule(ctx context.Context, id int64, delta int) error {
	r, err := s.GetRule(ctx, id)
	if err != nil {
		return err
	}
	rules, err := s.ListRules(ctx, r.Scope, r.OwnerID)
	if err != nil {
		return err
	}
	idx := -1
	for i := range rules {
		if rules[i].ID == id {
			idx = i
			break
		}
	}
	target := idx + delta
	if idx < 0 || target < 0 || target >= len(rules) {
		return nil
	}
	rules[idx], rules[target] = rules[target], rules[idx]

	c, cancel := ctxTimeout(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range rules {
		if _, err := tx.ExecContext(c, "UPDATE rules SET order_index = ? WHERE id = ?", i, rules[i].ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// checkSingleLine rejects embedded newlines, which would let a value inject
// unrelated directives into the generated configuration.
func checkSingleLine(field, v string) error {
	if strings.ContainsAny(v, "\n\r") {
		return fmt.Errorf("%s must be a single line", field)
	}
	return nil
}
