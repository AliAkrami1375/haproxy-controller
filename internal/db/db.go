// Package db opens the controller's SQLite database and applies migrations.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, keeps the binary cgo-free
)

// DB wraps *sql.DB with the controller's connection policy.
type DB struct {
	*sql.DB
}

// Open opens (creating if needed) the SQLite database at path, applies
// pragmas suited to a low-concurrency admin application, and migrates the
// schema to the current version.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		url.PathEscape(path))

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite handles one writer at a time; a small pool avoids lock churn.
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	d := &DB{DB: sqlDB}
	if err := d.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return d, nil
}

// migrate applies every migration whose version exceeds user_version, inside a
// transaction, then bumps user_version.
func (d *DB) migrate(ctx context.Context) error {
	var current int
	if err := d.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}

	for i, stmt := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		// PRAGMA does not accept bound parameters.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d set version: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d commit: %w", version, err)
		}
	}
	return nil
}

// migrations is an append-only list. Never edit an entry that has shipped:
// add a new one instead.
var migrations = []string{
	// 1: initial schema
	`
-- ---------------------------------------------------------------- identity
CREATE TABLE users (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    username        TEXT    NOT NULL UNIQUE,
    password_hash   TEXT    NOT NULL,
    full_name       TEXT    NOT NULL DEFAULT '',
    email           TEXT    NOT NULL DEFAULT '',
    role            TEXT    NOT NULL DEFAULT 'operator',
    is_active       INTEGER NOT NULL DEFAULT 1,
    must_change_pw  INTEGER NOT NULL DEFAULT 0,
    failed_attempts INTEGER NOT NULL DEFAULT 0,
    locked_until    TEXT,
    last_login_at   TEXT,
    last_login_ip   TEXT NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE sessions (
    id           TEXT    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   TEXT    NOT NULL,
    csrf_token   TEXT    NOT NULL,
    ip           TEXT    NOT NULL DEFAULT '',
    user_agent   TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    last_seen_at TEXT    NOT NULL DEFAULT (datetime('now')),
    expires_at   TEXT    NOT NULL
);
CREATE INDEX idx_sessions_user    ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER,
    username   TEXT    NOT NULL DEFAULT 'system',
    action     TEXT    NOT NULL,
    entity     TEXT    NOT NULL DEFAULT '',
    entity_id  TEXT    NOT NULL DEFAULT '',
    detail     TEXT    NOT NULL DEFAULT '',
    ip         TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_audit_created ON audit_log(created_at DESC);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ------------------------------------------------------- haproxy: global
CREATE TABLE global_config (
    id                        INTEGER PRIMARY KEY CHECK (id = 1),
    maxconn                   INTEGER NOT NULL DEFAULT 20000,
    nbthread                  INTEGER NOT NULL DEFAULT 0,
    run_user                  TEXT    NOT NULL DEFAULT 'haproxy',
    run_group                 TEXT    NOT NULL DEFAULT 'haproxy',
    chroot                    TEXT    NOT NULL DEFAULT '',
    daemon                    INTEGER NOT NULL DEFAULT 1,
    log_targets               TEXT    NOT NULL DEFAULT '/dev/log local0',
    stats_socket              TEXT    NOT NULL DEFAULT '/run/haproxy/admin.sock mode 660 level admin',
    stats_timeout             TEXT    NOT NULL DEFAULT '30s',
    hard_stop_after           TEXT    NOT NULL DEFAULT '',
    ssl_default_bind_ciphers  TEXT    NOT NULL DEFAULT 'ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305',
    ssl_default_bind_ciphersuites TEXT NOT NULL DEFAULT 'TLS_AES_128_GCM_SHA256:TLS_AES_256_GCM_SHA384:TLS_CHACHA20_POLY1305_SHA256',
    ssl_default_bind_options   TEXT   NOT NULL DEFAULT 'ssl-min-ver TLSv1.2 no-tls-tickets',
    ssl_default_server_ciphers TEXT   NOT NULL DEFAULT '',
    ssl_default_server_options TEXT   NOT NULL DEFAULT 'ssl-min-ver TLSv1.2',
    tune_ssl_default_dh_param  INTEGER NOT NULL DEFAULT 2048,
    ssl_dh_param_file          TEXT   NOT NULL DEFAULT '',
    extra                      TEXT   NOT NULL DEFAULT '',
    updated_at                 TEXT   NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE defaults_config (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    name                    TEXT    NOT NULL DEFAULT '',
    enabled                 INTEGER NOT NULL DEFAULT 1,
    mode                    TEXT    NOT NULL DEFAULT 'http',
    log_global              INTEGER NOT NULL DEFAULT 1,
    -- newline-separated bare option names; seeded by the bootstrap code
    options                 TEXT    NOT NULL DEFAULT '',
    retries                 INTEGER NOT NULL DEFAULT 3,
    maxconn                 INTEGER NOT NULL DEFAULT 0,
    timeout_connect         TEXT    NOT NULL DEFAULT '5s',
    timeout_client          TEXT    NOT NULL DEFAULT '50s',
    timeout_server          TEXT    NOT NULL DEFAULT '50s',
    timeout_http_request    TEXT    NOT NULL DEFAULT '10s',
    timeout_http_keep_alive TEXT    NOT NULL DEFAULT '10s',
    timeout_queue           TEXT    NOT NULL DEFAULT '30s',
    timeout_check           TEXT    NOT NULL DEFAULT '5s',
    timeout_tunnel          TEXT    NOT NULL DEFAULT '1h',
    compression             TEXT    NOT NULL DEFAULT '',
    error_files_ref         TEXT    NOT NULL DEFAULT '',
    extra                   TEXT    NOT NULL DEFAULT '',
    order_index             INTEGER NOT NULL DEFAULT 0
);

-- ---------------------------------------------------- haproxy: frontends
CREATE TABLE frontends (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT    NOT NULL UNIQUE,
    description         TEXT    NOT NULL DEFAULT '',
    enabled             INTEGER NOT NULL DEFAULT 1,
    mode                TEXT    NOT NULL DEFAULT 'http',
    default_backend_id  INTEGER REFERENCES backends(id) ON DELETE SET NULL,
    maxconn             INTEGER NOT NULL DEFAULT 0,
    option_forwardfor   INTEGER NOT NULL DEFAULT 1,
    option_httplog      INTEGER NOT NULL DEFAULT 1,
    option_http_close   INTEGER NOT NULL DEFAULT 0,
    force_https         INTEGER NOT NULL DEFAULT 0,
    hsts_enabled        INTEGER NOT NULL DEFAULT 0,
    hsts_max_age        INTEGER NOT NULL DEFAULT 31536000,
    hsts_subdomains     INTEGER NOT NULL DEFAULT 0,
    hsts_preload        INTEGER NOT NULL DEFAULT 0,
    rate_limit_enabled  INTEGER NOT NULL DEFAULT 0,
    rate_limit_rps      INTEGER NOT NULL DEFAULT 0,
    rate_limit_period   TEXT    NOT NULL DEFAULT '10s',
    stats_enabled       INTEGER NOT NULL DEFAULT 0,
    stats_uri           TEXT    NOT NULL DEFAULT '/haproxy-stats',
    stats_auth          TEXT    NOT NULL DEFAULT '',
    http_errors_ref     TEXT    NOT NULL DEFAULT '',
    log_settings        TEXT    NOT NULL DEFAULT '',
    extra               TEXT    NOT NULL DEFAULT '',
    order_index         INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE binds (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    frontend_id  INTEGER NOT NULL REFERENCES frontends(id) ON DELETE CASCADE,
    address      TEXT    NOT NULL DEFAULT '*',
    port         INTEGER NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    ssl          INTEGER NOT NULL DEFAULT 0,
    cert_source  TEXT    NOT NULL DEFAULT 'dir',
    cert_ref     TEXT    NOT NULL DEFAULT '',
    alpn         TEXT    NOT NULL DEFAULT 'h2,http/1.1',
    accept_proxy INTEGER NOT NULL DEFAULT 0,
    transparent  INTEGER NOT NULL DEFAULT 0,
    extra_params TEXT    NOT NULL DEFAULT '',
    order_index  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_binds_frontend ON binds(frontend_id);

-- ----------------------------------------------------- haproxy: backends
CREATE TABLE backends (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT    NOT NULL UNIQUE,
    description        TEXT    NOT NULL DEFAULT '',
    enabled            INTEGER NOT NULL DEFAULT 1,
    mode               TEXT    NOT NULL DEFAULT 'http',
    balance            TEXT    NOT NULL DEFAULT 'roundrobin',
    balance_param      TEXT    NOT NULL DEFAULT '',
    option_forwardfor  INTEGER NOT NULL DEFAULT 1,
    option_http_close  INTEGER NOT NULL DEFAULT 0,
    httpchk_enabled    INTEGER NOT NULL DEFAULT 0,
    httpchk_method     TEXT    NOT NULL DEFAULT 'GET',
    httpchk_uri        TEXT    NOT NULL DEFAULT '/',
    httpchk_version    TEXT    NOT NULL DEFAULT 'HTTP/1.1',
    httpchk_host       TEXT    NOT NULL DEFAULT '',
    check_expect       TEXT    NOT NULL DEFAULT '',
    tcpchk_enabled     INTEGER NOT NULL DEFAULT 0,
    cookie_name        TEXT    NOT NULL DEFAULT '',
    cookie_options     TEXT    NOT NULL DEFAULT 'insert indirect nocache',
    stick_enabled      INTEGER NOT NULL DEFAULT 0,
    stick_table        TEXT    NOT NULL DEFAULT '',
    stick_on           TEXT    NOT NULL DEFAULT '',
    retries            INTEGER NOT NULL DEFAULT 0,
    timeout_connect    TEXT    NOT NULL DEFAULT '',
    timeout_server     TEXT    NOT NULL DEFAULT '',
    timeout_check      TEXT    NOT NULL DEFAULT '',
    http_errors_ref    TEXT    NOT NULL DEFAULT '',
    extra              TEXT    NOT NULL DEFAULT '',
    order_index        INTEGER NOT NULL DEFAULT 0,
    created_at         TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE servers (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    backend_id   INTEGER NOT NULL REFERENCES backends(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    address      TEXT    NOT NULL,
    port         INTEGER NOT NULL DEFAULT 0,
    enabled      INTEGER NOT NULL DEFAULT 1,
    weight       INTEGER NOT NULL DEFAULT 100,
    maxconn      INTEGER NOT NULL DEFAULT 0,
    check_enabled INTEGER NOT NULL DEFAULT 1,
    check_inter  TEXT    NOT NULL DEFAULT '2s',
    check_rise   INTEGER NOT NULL DEFAULT 2,
    check_fall   INTEGER NOT NULL DEFAULT 3,
    ssl          INTEGER NOT NULL DEFAULT 0,
    ssl_verify   TEXT    NOT NULL DEFAULT 'none',
    sni          TEXT    NOT NULL DEFAULT '',
    backup       INTEGER NOT NULL DEFAULT 0,
    send_proxy   TEXT    NOT NULL DEFAULT '',
    cookie_value TEXT    NOT NULL DEFAULT '',
    extra_params TEXT    NOT NULL DEFAULT '',
    order_index  INTEGER NOT NULL DEFAULT 0,
    UNIQUE (backend_id, name)
);
CREATE INDEX idx_servers_backend ON servers(backend_id);

-- ------------------------------------------------- routing: domains/ACLs
CREATE TABLE domains (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    hostname     TEXT    NOT NULL,
    match_type   TEXT    NOT NULL DEFAULT 'exact',
    path_prefix  TEXT    NOT NULL DEFAULT '',
    frontend_id  INTEGER NOT NULL REFERENCES frontends(id) ON DELETE CASCADE,
    backend_id   INTEGER REFERENCES backends(id) ON DELETE SET NULL,
    redirect_to  TEXT    NOT NULL DEFAULT '',
    redirect_code INTEGER NOT NULL DEFAULT 301,
    force_https  INTEGER NOT NULL DEFAULT 0,
    enabled      INTEGER NOT NULL DEFAULT 1,
    order_index  INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT    NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_domains_frontend ON domains(frontend_id);

CREATE TABLE acls (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scope       TEXT    NOT NULL DEFAULT 'frontend',
    owner_id    INTEGER NOT NULL,
    name        TEXT    NOT NULL,
    expression  TEXT    NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    order_index INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_acls_owner ON acls(scope, owner_id);

CREATE TABLE rules (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scope       TEXT    NOT NULL DEFAULT 'frontend',
    owner_id    INTEGER NOT NULL,
    directive   TEXT    NOT NULL,
    argument    TEXT    NOT NULL DEFAULT '',
    condition   TEXT    NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    order_index INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_rules_owner ON rules(scope, owner_id);

-- ------------------------------------------------------------- TLS certs
CREATE TABLE certificates (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    file_name    TEXT    NOT NULL,
    domains      TEXT    NOT NULL DEFAULT '',
    subject      TEXT    NOT NULL DEFAULT '',
    issuer       TEXT    NOT NULL DEFAULT '',
    serial       TEXT    NOT NULL DEFAULT '',
    fingerprint  TEXT    NOT NULL DEFAULT '',
    not_before   TEXT    NOT NULL DEFAULT '',
    not_after    TEXT    NOT NULL DEFAULT '',
    cert_pem     TEXT    NOT NULL,
    key_pem      TEXT    NOT NULL,
    chain_pem    TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- ---------------------------------------------------------- error pages
CREATE TABLE error_pages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    group_name   TEXT    NOT NULL DEFAULT 'default',
    status_code  INTEGER NOT NULL,
    content_type TEXT    NOT NULL DEFAULT 'text/html; charset=utf-8',
    headers      TEXT    NOT NULL DEFAULT '',
    body         TEXT    NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT    NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_error_pages_group ON error_pages(group_name);

-- -------------------------------------------------- free-form config bits
CREATE TABLE snippets (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    section_type TEXT    NOT NULL DEFAULT 'raw',
    section_arg  TEXT    NOT NULL DEFAULT '',
    body         TEXT    NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    order_index  INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE (section_type, name)
);

-- ------------------------------------------------------ config versions
CREATE TABLE config_versions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    content     TEXT    NOT NULL,
    checksum    TEXT    NOT NULL,
    status      TEXT    NOT NULL DEFAULT 'applied',
    comment     TEXT    NOT NULL DEFAULT '',
    created_by  TEXT    NOT NULL DEFAULT 'system',
    error       TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);
 CREATE INDEX idx_versions_created ON config_versions(created_at DESC);
`,
	// 2: assistant conversations
	`
CREATE TABLE ai_conversations (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    title      TEXT    NOT NULL DEFAULT 'New conversation',
    created_at TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT    NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_ai_conversations_user ON ai_conversations(user_id, updated_at DESC);

CREATE TABLE ai_messages (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id INTEGER NOT NULL REFERENCES ai_conversations(id) ON DELETE CASCADE,
    role            TEXT    NOT NULL,          -- user | assistant
    content         TEXT    NOT NULL DEFAULT '',
    steps           TEXT    NOT NULL DEFAULT '',  -- JSON array of agent steps
    transcript      TEXT    NOT NULL DEFAULT '',  -- JSON model messages for continuation
    changed         INTEGER NOT NULL DEFAULT 0,
    valid           INTEGER NOT NULL DEFAULT 1,
    tokens          INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_ai_messages_conversation ON ai_messages(conversation_id, id);
`,
}
