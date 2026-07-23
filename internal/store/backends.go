package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const backendColumns = `id, name, description, enabled, mode, balance, balance_param,
	option_forwardfor, option_http_close,
	httpchk_enabled, httpchk_method, httpchk_uri, httpchk_version, httpchk_host, check_expect,
	tcpchk_enabled, cookie_name, cookie_options, stick_enabled, stick_table, stick_on,
	retries, timeout_connect, timeout_server, timeout_check, http_errors_ref, extra,
	order_index, created_at, updated_at`

func scanBackend(row interface{ Scan(...any) error }) (*Backend, error) {
	var (
		b                  Backend
		created, updated   sql.NullString
		enabled, fwdfor    int
		httpclose, httpchk int
		tcpchk, stick      int
	)
	err := row.Scan(&b.ID, &b.Name, &b.Description, &enabled, &b.Mode, &b.Balance, &b.BalanceParam,
		&fwdfor, &httpclose,
		&httpchk, &b.HTTPChkMethod, &b.HTTPChkURI, &b.HTTPChkVersion, &b.HTTPChkHost, &b.CheckExpect,
		&tcpchk, &b.CookieName, &b.CookieOptions, &stick, &b.StickTable, &b.StickOn,
		&b.Retries, &b.TimeoutConnect, &b.TimeoutServer, &b.TimeoutCheck, &b.HTTPErrorsRef, &b.Extra,
		&b.OrderIndex, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	b.Enabled = enabled != 0
	b.OptionForwardFor = fwdfor != 0
	b.OptionHTTPClose = httpclose != 0
	b.HTTPChkEnabled = httpchk != 0
	b.TCPChkEnabled = tcpchk != 0
	b.StickEnabled = stick != 0
	b.CreatedAt = parseTime(created)
	b.UpdatedAt = parseTime(updated)
	return &b, nil
}

// ListBackends returns every backend in render order, without servers.
func (s *Store) ListBackends(ctx context.Context) ([]*Backend, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, "SELECT "+backendColumns+" FROM backends ORDER BY order_index, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Backend
	for rows.Next() {
		b, err := scanBackend(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBackend loads one backend without servers.
func (s *Store) GetBackend(ctx context.Context, id int64) (*Backend, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanBackend(s.db.QueryRowContext(c, "SELECT "+backendColumns+" FROM backends WHERE id = ?", id))
}

// GetBackendFull loads a backend together with its servers, ACLs and rules.
func (s *Store) GetBackendFull(ctx context.Context, id int64) (*Backend, error) {
	b, err := s.GetBackend(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.Servers, err = s.ListServers(ctx, id); err != nil {
		return nil, err
	}
	if b.ACLs, err = s.ListACLs(ctx, "backend", id); err != nil {
		return nil, err
	}
	if b.Rules, err = s.ListRules(ctx, "backend", id); err != nil {
		return nil, err
	}
	return b, nil
}

// validBalance is the set of load balancing algorithms the editor offers.
var validBalance = map[string]bool{
	"roundrobin": true, "static-rr": true, "leastconn": true, "first": true,
	"source": true, "uri": true, "url_param": true, "hdr": true,
	"random": true, "rdp-cookie": true,
}

// SaveBackend inserts or updates a backend and returns its id.
func (s *Store) SaveBackend(ctx context.Context, b *Backend) (int64, error) {
	if err := ValidateName("backend", b.Name); err != nil {
		return 0, err
	}
	if b.Mode != "http" && b.Mode != "tcp" {
		return 0, fmt.Errorf("backend mode must be http or tcp, got %q", b.Mode)
	}
	if !validBalance[b.Balance] {
		return 0, fmt.Errorf("unknown balance algorithm %q", b.Balance)
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	args := []any{
		b.Name, b.Description, boolToInt(b.Enabled), b.Mode, b.Balance, b.BalanceParam,
		boolToInt(b.OptionForwardFor), boolToInt(b.OptionHTTPClose),
		boolToInt(b.HTTPChkEnabled), b.HTTPChkMethod, b.HTTPChkURI, b.HTTPChkVersion,
		b.HTTPChkHost, b.CheckExpect,
		boolToInt(b.TCPChkEnabled), b.CookieName, b.CookieOptions,
		boolToInt(b.StickEnabled), b.StickTable, b.StickOn,
		b.Retries, b.TimeoutConnect, b.TimeoutServer, b.TimeoutCheck, b.HTTPErrorsRef, b.Extra,
		b.OrderIndex,
	}

	if b.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO backends (name, description, enabled, mode, balance, balance_param,
				option_forwardfor, option_http_close,
				httpchk_enabled, httpchk_method, httpchk_uri, httpchk_version, httpchk_host, check_expect,
				tcpchk_enabled, cookie_name, cookie_options, stick_enabled, stick_table, stick_on,
				retries, timeout_connect, timeout_server, timeout_check, http_errors_ref, extra, order_index)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
		if err != nil {
			if isUniqueViolation(err) {
				return 0, fmt.Errorf("a backend named %q already exists", b.Name)
			}
			return 0, err
		}
		return res.LastInsertId()
	}

	args = append(args, b.ID)
	_, err := s.db.ExecContext(c, `
		UPDATE backends SET name=?, description=?, enabled=?, mode=?, balance=?, balance_param=?,
			option_forwardfor=?, option_http_close=?,
			httpchk_enabled=?, httpchk_method=?, httpchk_uri=?, httpchk_version=?, httpchk_host=?, check_expect=?,
			tcpchk_enabled=?, cookie_name=?, cookie_options=?, stick_enabled=?, stick_table=?, stick_on=?,
			retries=?, timeout_connect=?, timeout_server=?, timeout_check=?, http_errors_ref=?, extra=?,
			order_index=?, updated_at=datetime('now')
		WHERE id = ?`, args...)
	if err != nil && isUniqueViolation(err) {
		return 0, fmt.Errorf("a backend named %q already exists", b.Name)
	}
	return b.ID, err
}

// BackendReferences reports where a backend is still used, so deletion can be
// refused with a helpful message instead of silently breaking routing.
func (s *Store) BackendReferences(ctx context.Context, id int64) ([]string, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	var refs []string
	rows, err := s.db.QueryContext(c,
		"SELECT name FROM frontends WHERE default_backend_id = ?", id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, "default backend of frontend "+n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(c, "SELECT hostname FROM domains WHERE backend_id = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		refs = append(refs, "route for domain "+h)
	}
	return refs, rows.Err()
}

// DeleteBackend removes a backend and its servers, ACLs and rules.
func (s *Store) DeleteBackend(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(c, "DELETE FROM acls WHERE scope = 'backend' AND owner_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(c, "DELETE FROM rules WHERE scope = 'backend' AND owner_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(c, "DELETE FROM backends WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- servers

const serverColumns = `id, backend_id, name, address, port, enabled, weight, maxconn,
	check_enabled, check_inter, check_rise, check_fall, ssl, ssl_verify, sni,
	backup, send_proxy, cookie_value, extra_params, order_index`

func scanServer(row interface{ Scan(...any) error }) (*Server, error) {
	var (
		sv                  Server
		enabled, check, ssl int
		backup              int
	)
	err := row.Scan(&sv.ID, &sv.BackendID, &sv.Name, &sv.Address, &sv.Port, &enabled,
		&sv.Weight, &sv.Maxconn, &check, &sv.CheckInter, &sv.CheckRise, &sv.CheckFall,
		&ssl, &sv.SSLVerify, &sv.SNI, &backup, &sv.SendProxy, &sv.CookieValue,
		&sv.ExtraParams, &sv.OrderIndex)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	sv.Enabled = enabled != 0
	sv.CheckEnabled = check != 0
	sv.SSL = ssl != 0
	sv.Backup = backup != 0
	return &sv, nil
}

// ListServers returns a backend's servers in render order.
func (s *Store) ListServers(ctx context.Context, backendID int64) ([]Server, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c,
		"SELECT "+serverColumns+" FROM servers WHERE backend_id = ? ORDER BY order_index, id", backendID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Server
	for rows.Next() {
		sv, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sv)
	}
	return out, rows.Err()
}

// GetServer loads a single server line.
func (s *Store) GetServer(ctx context.Context, id int64) (*Server, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanServer(s.db.QueryRowContext(c, "SELECT "+serverColumns+" FROM servers WHERE id = ?", id))
}

// SaveServer inserts or updates a server line.
func (s *Store) SaveServer(ctx context.Context, sv *Server) (int64, error) {
	if err := ValidateName("server", sv.Name); err != nil {
		return 0, err
	}
	if strings.TrimSpace(sv.Address) == "" {
		return 0, errors.New("server address is required")
	}
	if strings.ContainsAny(sv.Address, " \t\n") {
		return 0, errors.New("server address must not contain whitespace")
	}
	if sv.Port < 0 || sv.Port > 65535 {
		return 0, fmt.Errorf("server port %d is out of range (0-65535)", sv.Port)
	}
	if sv.Weight < 0 || sv.Weight > 256 {
		return 0, fmt.Errorf("server weight %d is out of range (0-256)", sv.Weight)
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	args := []any{sv.BackendID, sv.Name, sv.Address, sv.Port, boolToInt(sv.Enabled),
		sv.Weight, sv.Maxconn, boolToInt(sv.CheckEnabled), sv.CheckInter, sv.CheckRise, sv.CheckFall,
		boolToInt(sv.SSL), sv.SSLVerify, sv.SNI, boolToInt(sv.Backup), sv.SendProxy,
		sv.CookieValue, sv.ExtraParams, sv.OrderIndex}

	if sv.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO servers (backend_id, name, address, port, enabled, weight, maxconn,
				check_enabled, check_inter, check_rise, check_fall, ssl, ssl_verify, sni,
				backup, send_proxy, cookie_value, extra_params, order_index)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
		if err != nil {
			if isUniqueViolation(err) {
				return 0, fmt.Errorf("this backend already has a server named %q", sv.Name)
			}
			return 0, err
		}
		return res.LastInsertId()
	}

	args = append(args, sv.ID)
	_, err := s.db.ExecContext(c, `
		UPDATE servers SET backend_id=?, name=?, address=?, port=?, enabled=?, weight=?, maxconn=?,
			check_enabled=?, check_inter=?, check_rise=?, check_fall=?, ssl=?, ssl_verify=?, sni=?,
			backup=?, send_proxy=?, cookie_value=?, extra_params=?, order_index=?
		WHERE id = ?`, args...)
	if err != nil && isUniqueViolation(err) {
		return 0, fmt.Errorf("this backend already has a server named %q", sv.Name)
	}
	return sv.ID, err
}

// DeleteServer removes a server line.
func (s *Store) DeleteServer(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM servers WHERE id = ?", id)
	return err
}

// CountServers returns the total number of configured servers.
func (s *Store) CountServers(ctx context.Context) (int, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	var n int
	err := s.db.QueryRowContext(c, "SELECT COUNT(*) FROM servers").Scan(&n)
	return n, err
}
