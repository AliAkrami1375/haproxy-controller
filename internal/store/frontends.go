package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const frontendColumns = `id, name, description, enabled, mode, default_backend_id, maxconn,
	option_forwardfor, option_httplog, option_http_close, force_https,
	hsts_enabled, hsts_max_age, hsts_subdomains, hsts_preload,
	rate_limit_enabled, rate_limit_rps, rate_limit_period,
	stats_enabled, stats_uri, stats_auth, http_errors_ref, log_settings, extra,
	order_index, created_at, updated_at`

func scanFrontend(row interface{ Scan(...any) error }) (*Frontend, error) {
	var (
		f                    Frontend
		defBackend           sql.NullInt64
		created, updated     sql.NullString
		enabled, fwdfor      int
		httplog, httpclose   int
		forceHTTPS, hsts     int
		hstsSub, hstsPreload int
		rlEnabled, stats     int
	)
	err := row.Scan(&f.ID, &f.Name, &f.Description, &enabled, &f.Mode, &defBackend, &f.Maxconn,
		&fwdfor, &httplog, &httpclose, &forceHTTPS,
		&hsts, &f.HSTSMaxAge, &hstsSub, &hstsPreload,
		&rlEnabled, &f.RateLimitRPS, &f.RateLimitPeriod,
		&stats, &f.StatsURI, &f.StatsAuth, &f.HTTPErrorsRef, &f.LogSettings, &f.Extra,
		&f.OrderIndex, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if defBackend.Valid {
		id := defBackend.Int64
		f.DefaultBackendID = &id
	}
	f.Enabled = enabled != 0
	f.OptionForwardFor = fwdfor != 0
	f.OptionHTTPLog = httplog != 0
	f.OptionHTTPClose = httpclose != 0
	f.ForceHTTPS = forceHTTPS != 0
	f.HSTSEnabled = hsts != 0
	f.HSTSSubdomains = hstsSub != 0
	f.HSTSPreload = hstsPreload != 0
	f.RateLimitEnabled = rlEnabled != 0
	f.StatsEnabled = stats != 0
	f.CreatedAt = parseTime(created)
	f.UpdatedAt = parseTime(updated)
	return &f, nil
}

// ListFrontends returns every frontend in render order, without children.
func (s *Store) ListFrontends(ctx context.Context) ([]*Frontend, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, "SELECT "+frontendColumns+" FROM frontends ORDER BY order_index, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Frontend
	for rows.Next() {
		f, err := scanFrontend(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFrontend loads one frontend without children.
func (s *Store) GetFrontend(ctx context.Context, id int64) (*Frontend, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanFrontend(s.db.QueryRowContext(c, "SELECT "+frontendColumns+" FROM frontends WHERE id = ?", id))
}

// GetFrontendFull loads a frontend together with its binds, ACLs, rules and
// domain routes.
func (s *Store) GetFrontendFull(ctx context.Context, id int64) (*Frontend, error) {
	f, err := s.GetFrontend(ctx, id)
	if err != nil {
		return nil, err
	}
	if f.Binds, err = s.ListBinds(ctx, id); err != nil {
		return nil, err
	}
	if f.ACLs, err = s.ListACLs(ctx, "frontend", id); err != nil {
		return nil, err
	}
	if f.Rules, err = s.ListRules(ctx, "frontend", id); err != nil {
		return nil, err
	}
	if f.Domains, err = s.ListDomainsByFrontend(ctx, id); err != nil {
		return nil, err
	}
	return f, nil
}

// SaveFrontend inserts or updates a frontend and returns its id.
func (s *Store) SaveFrontend(ctx context.Context, f *Frontend) (int64, error) {
	if err := ValidateName("frontend", f.Name); err != nil {
		return 0, err
	}
	if f.Mode != "http" && f.Mode != "tcp" {
		return 0, fmt.Errorf("frontend mode must be http or tcp, got %q", f.Mode)
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	args := []any{
		f.Name, f.Description, boolToInt(f.Enabled), f.Mode, nullInt64(f.DefaultBackendID), f.Maxconn,
		boolToInt(f.OptionForwardFor), boolToInt(f.OptionHTTPLog), boolToInt(f.OptionHTTPClose),
		boolToInt(f.ForceHTTPS), boolToInt(f.HSTSEnabled), f.HSTSMaxAge,
		boolToInt(f.HSTSSubdomains), boolToInt(f.HSTSPreload),
		boolToInt(f.RateLimitEnabled), f.RateLimitRPS, f.RateLimitPeriod,
		boolToInt(f.StatsEnabled), f.StatsURI, f.StatsAuth, f.HTTPErrorsRef, f.LogSettings, f.Extra,
		f.OrderIndex,
	}

	if f.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO frontends (name, description, enabled, mode, default_backend_id, maxconn,
				option_forwardfor, option_httplog, option_http_close, force_https,
				hsts_enabled, hsts_max_age, hsts_subdomains, hsts_preload,
				rate_limit_enabled, rate_limit_rps, rate_limit_period,
				stats_enabled, stats_uri, stats_auth, http_errors_ref, log_settings, extra, order_index)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
		if err != nil {
			if isUniqueViolation(err) {
				return 0, fmt.Errorf("a frontend named %q already exists", f.Name)
			}
			return 0, err
		}
		return res.LastInsertId()
	}

	args = append(args, f.ID)
	_, err := s.db.ExecContext(c, `
		UPDATE frontends SET name=?, description=?, enabled=?, mode=?, default_backend_id=?, maxconn=?,
			option_forwardfor=?, option_httplog=?, option_http_close=?, force_https=?,
			hsts_enabled=?, hsts_max_age=?, hsts_subdomains=?, hsts_preload=?,
			rate_limit_enabled=?, rate_limit_rps=?, rate_limit_period=?,
			stats_enabled=?, stats_uri=?, stats_auth=?, http_errors_ref=?, log_settings=?, extra=?,
			order_index=?, updated_at=datetime('now')
		WHERE id = ?`, args...)
	if err != nil && isUniqueViolation(err) {
		return 0, fmt.Errorf("a frontend named %q already exists", f.Name)
	}
	return f.ID, err
}

// DeleteFrontend removes a frontend and everything attached to it.
func (s *Store) DeleteFrontend(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Binds and domains cascade; ACLs and rules are keyed by (scope, owner_id)
	// so they need an explicit delete.
	if _, err := tx.ExecContext(c, "DELETE FROM acls WHERE scope = 'frontend' AND owner_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(c, "DELETE FROM rules WHERE scope = 'frontend' AND owner_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(c, "DELETE FROM frontends WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// ------------------------------------------------------------------ binds

const bindColumns = `id, frontend_id, address, port, enabled, ssl, cert_source, cert_ref,
	alpn, accept_proxy, transparent, extra_params, order_index`

// ListBinds returns a frontend's bind lines in render order.
func (s *Store) ListBinds(ctx context.Context, frontendID int64) ([]Bind, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c,
		"SELECT "+bindColumns+" FROM binds WHERE frontend_id = ? ORDER BY order_index, id", frontendID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Bind
	for rows.Next() {
		var (
			b                              Bind
			enabled, ssl, accept, transpar int
		)
		if err := rows.Scan(&b.ID, &b.FrontendID, &b.Address, &b.Port, &enabled, &ssl,
			&b.CertSource, &b.CertRef, &b.ALPN, &accept, &transpar, &b.ExtraParams, &b.OrderIndex); err != nil {
			return nil, err
		}
		b.Enabled = enabled != 0
		b.SSL = ssl != 0
		b.AcceptProxy = accept != 0
		b.Transparent = transpar != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBind loads a single bind line.
func (s *Store) GetBind(ctx context.Context, id int64) (*Bind, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	var (
		b                              Bind
		enabled, ssl, accept, transpar int
	)
	err := s.db.QueryRowContext(c, "SELECT "+bindColumns+" FROM binds WHERE id = ?", id).
		Scan(&b.ID, &b.FrontendID, &b.Address, &b.Port, &enabled, &ssl,
			&b.CertSource, &b.CertRef, &b.ALPN, &accept, &transpar, &b.ExtraParams, &b.OrderIndex)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	b.Enabled = enabled != 0
	b.SSL = ssl != 0
	b.AcceptProxy = accept != 0
	b.Transparent = transpar != 0
	return &b, nil
}

// SaveBind inserts or updates a bind line.
func (s *Store) SaveBind(ctx context.Context, b *Bind) (int64, error) {
	if b.Port < 1 || b.Port > 65535 {
		return 0, fmt.Errorf("bind port %d is out of range (1-65535)", b.Port)
	}
	if strings.ContainsAny(b.Address, " \t\n") {
		return 0, errors.New("bind address must not contain whitespace")
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	args := []any{b.FrontendID, b.Address, b.Port, boolToInt(b.Enabled), boolToInt(b.SSL),
		b.CertSource, b.CertRef, b.ALPN, boolToInt(b.AcceptProxy), boolToInt(b.Transparent),
		b.ExtraParams, b.OrderIndex}

	if b.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO binds (frontend_id, address, port, enabled, ssl, cert_source, cert_ref,
				alpn, accept_proxy, transparent, extra_params, order_index)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}

	args = append(args, b.ID)
	_, err := s.db.ExecContext(c, `
		UPDATE binds SET frontend_id=?, address=?, port=?, enabled=?, ssl=?, cert_source=?, cert_ref=?,
			alpn=?, accept_proxy=?, transparent=?, extra_params=?, order_index=?
		WHERE id = ?`, args...)
	return b.ID, err
}

// DeleteBind removes a bind line.
func (s *Store) DeleteBind(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM binds WHERE id = ?", id)
	return err
}

// PortsInUse maps "address:port" to the frontend name that binds it, so the
// UI can warn about collisions, including with the controller's own port.
func (s *Store) PortsInUse(ctx context.Context) (map[string]string, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, `
		SELECT f.name, b.address, b.port FROM binds b
		JOIN frontends f ON f.id = b.frontend_id
		WHERE b.enabled = 1 AND f.enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, addr string
		var port int
		if err := rows.Scan(&name, &addr, &port); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%s:%d", addr, port)] = name
	}
	return out, rows.Err()
}
