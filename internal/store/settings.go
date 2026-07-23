package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// Setting keys used by the controller. Anything the operator can change from
// the Settings page but that is not process-level lives here.
const (
	SetPanelTitle       = "panel.title"
	SetPanelFooterNote  = "panel.footer_note"
	SetSessionTTLMins   = "security.session_ttl_minutes"
	SetIdleLogoutMins   = "security.idle_logout_minutes"
	SetAutoBackupKeep   = "deploy.keep_versions"
	SetValidateOnSave   = "deploy.validate_on_save"
	SetRollbackOnFail   = "deploy.rollback_on_failure"
	SetReloadStrategy   = "deploy.reload_strategy"
	SetConfigHeaderNote = "config.header_note"

	// Assistant (OpenRouter) settings.
	SetAIEnabled = "ai.enabled"
	SetAIAPIKey  = "ai.api_key" // stored encrypted
	SetAIModel   = "ai.model"
	SetAIBaseURL = "ai.base_url"
)

// GetSetting returns a stored setting or def when unset.
func (s *Store) GetSetting(ctx context.Context, key, def string) string {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	var v string
	err := s.db.QueryRowContext(c, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if err != nil || v == "" {
		return def
	}
	return v
}

// GetSettingInt returns an integer setting or def when unset/unparseable.
func (s *Store) GetSettingInt(ctx context.Context, key string, def int) int {
	raw := s.GetSetting(ctx, key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// GetSettingBool returns a boolean setting or def when unset.
func (s *Store) GetSettingBool(ctx context.Context, key string, def bool) bool {
	raw := s.GetSetting(ctx, key, "")
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = datetime('now')`,
		key, value)
	return err
}

// SetSettingBool stores a boolean setting.
func (s *Store) SetSettingBool(ctx context.Context, key string, v bool) error {
	if v {
		return s.SetSetting(ctx, key, "1")
	}
	return s.SetSetting(ctx, key, "0")
}

// AllSettings returns every stored setting as a map.
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, "SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- global

// GetGlobal loads the singleton `global` section configuration.
func (s *Store) GetGlobal(ctx context.Context) (*GlobalConfig, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	var g GlobalConfig
	var daemon int
	err := s.db.QueryRowContext(c, `
		SELECT maxconn, nbthread, run_user, run_group, chroot, daemon, log_targets,
		       stats_socket, stats_timeout, hard_stop_after,
		       ssl_default_bind_ciphers, ssl_default_bind_ciphersuites, ssl_default_bind_options,
		       ssl_default_server_ciphers, ssl_default_server_options,
		       tune_ssl_default_dh_param, ssl_dh_param_file, extra
		FROM global_config WHERE id = 1`).
		Scan(&g.Maxconn, &g.Nbthread, &g.RunUser, &g.RunGroup, &g.Chroot, &daemon, &g.LogTargets,
			&g.StatsSocket, &g.StatsTimeout, &g.HardStopAfter,
			&g.SSLDefaultBindCiphers, &g.SSLDefaultBindCiphersuites, &g.SSLDefaultBindOptions,
			&g.SSLDefaultServerCiphers, &g.SSLDefaultServerOptions,
			&g.TuneSSLDefaultDHParam, &g.SSLDHParamFile, &g.Extra)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	g.Daemon = daemon != 0
	return &g, nil
}

// SaveGlobal persists the `global` section configuration.
func (s *Store) SaveGlobal(ctx context.Context, g *GlobalConfig) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, `
		UPDATE global_config SET
			maxconn = ?, nbthread = ?, run_user = ?, run_group = ?, chroot = ?, daemon = ?,
			log_targets = ?, stats_socket = ?, stats_timeout = ?, hard_stop_after = ?,
			ssl_default_bind_ciphers = ?, ssl_default_bind_ciphersuites = ?, ssl_default_bind_options = ?,
			ssl_default_server_ciphers = ?, ssl_default_server_options = ?,
			tune_ssl_default_dh_param = ?, ssl_dh_param_file = ?, extra = ?,
			updated_at = datetime('now')
		WHERE id = 1`,
		g.Maxconn, g.Nbthread, g.RunUser, g.RunGroup, g.Chroot, boolToInt(g.Daemon),
		g.LogTargets, g.StatsSocket, g.StatsTimeout, g.HardStopAfter,
		g.SSLDefaultBindCiphers, g.SSLDefaultBindCiphersuites, g.SSLDefaultBindOptions,
		g.SSLDefaultServerCiphers, g.SSLDefaultServerOptions,
		g.TuneSSLDefaultDHParam, g.SSLDHParamFile, g.Extra)
	return err
}

// -------------------------------------------------------------- defaults

const defaultsColumns = `id, name, enabled, mode, log_global, options, retries, maxconn,
	timeout_connect, timeout_client, timeout_server, timeout_http_request,
	timeout_http_keep_alive, timeout_queue, timeout_check, timeout_tunnel,
	compression, error_files_ref, extra, order_index`

func scanDefaults(row interface{ Scan(...any) error }) (*DefaultsConfig, error) {
	var d DefaultsConfig
	var enabled, logGlobal int
	err := row.Scan(&d.ID, &d.Name, &enabled, &d.Mode, &logGlobal, &d.Options, &d.Retries, &d.Maxconn,
		&d.TimeoutConnect, &d.TimeoutClient, &d.TimeoutServer, &d.TimeoutHTTPRequest,
		&d.TimeoutHTTPKeepAlive, &d.TimeoutQueue, &d.TimeoutCheck, &d.TimeoutTunnel,
		&d.Compression, &d.ErrorFilesRef, &d.Extra, &d.OrderIndex)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	d.Enabled = enabled != 0
	d.LogGlobal = logGlobal != 0
	return &d, nil
}

// ListDefaults returns every `defaults` section in render order.
func (s *Store) ListDefaults(ctx context.Context) ([]*DefaultsConfig, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, "SELECT "+defaultsColumns+" FROM defaults_config ORDER BY order_index, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*DefaultsConfig
	for rows.Next() {
		d, err := scanDefaults(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDefaults loads one `defaults` section.
func (s *Store) GetDefaults(ctx context.Context, id int64) (*DefaultsConfig, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanDefaults(s.db.QueryRowContext(c, "SELECT "+defaultsColumns+" FROM defaults_config WHERE id = ?", id))
}

// SaveDefaults inserts or updates a `defaults` section.
func (s *Store) SaveDefaults(ctx context.Context, d *DefaultsConfig) (int64, error) {
	if d.Name != "" {
		if err := ValidateName("defaults", d.Name); err != nil {
			return 0, err
		}
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	if d.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO defaults_config (name, enabled, mode, log_global, options, retries, maxconn,
				timeout_connect, timeout_client, timeout_server, timeout_http_request,
				timeout_http_keep_alive, timeout_queue, timeout_check, timeout_tunnel,
				compression, error_files_ref, extra, order_index)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			d.Name, boolToInt(d.Enabled), d.Mode, boolToInt(d.LogGlobal), d.Options, d.Retries, d.Maxconn,
			d.TimeoutConnect, d.TimeoutClient, d.TimeoutServer, d.TimeoutHTTPRequest,
			d.TimeoutHTTPKeepAlive, d.TimeoutQueue, d.TimeoutCheck, d.TimeoutTunnel,
			d.Compression, d.ErrorFilesRef, d.Extra, d.OrderIndex)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}

	_, err := s.db.ExecContext(c, `
		UPDATE defaults_config SET name=?, enabled=?, mode=?, log_global=?, options=?, retries=?, maxconn=?,
			timeout_connect=?, timeout_client=?, timeout_server=?, timeout_http_request=?,
			timeout_http_keep_alive=?, timeout_queue=?, timeout_check=?, timeout_tunnel=?,
			compression=?, error_files_ref=?, extra=?, order_index=?
		WHERE id = ?`,
		d.Name, boolToInt(d.Enabled), d.Mode, boolToInt(d.LogGlobal), d.Options, d.Retries, d.Maxconn,
		d.TimeoutConnect, d.TimeoutClient, d.TimeoutServer, d.TimeoutHTTPRequest,
		d.TimeoutHTTPKeepAlive, d.TimeoutQueue, d.TimeoutCheck, d.TimeoutTunnel,
		d.Compression, d.ErrorFilesRef, d.Extra, d.OrderIndex, d.ID)
	return d.ID, err
}

// DeleteDefaults removes a `defaults` section.
func (s *Store) DeleteDefaults(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM defaults_config WHERE id = ?", id)
	return err
}
