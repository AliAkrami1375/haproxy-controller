// Package config loads runtime configuration for the controller process.
//
// Precedence, lowest to highest: built-in defaults, JSON config file,
// environment variables, command line flags. Values that the operator may
// change from the web UI (listen address/port, TLS material) are persisted
// back to the config file so they survive a restart.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultConfigPath is where the controller looks for its config file when
// neither -config nor HC_CONFIG is supplied.
const DefaultConfigPath = "/etc/haproxy-controller/controller.json"

// DefaultControlPort is the default port for the web control panel. It is
// deliberately outside the range operators normally hand to HAProxy frontends.
const DefaultControlPort = 9000

// Process modes. See Config.ProcessMode.
const (
	ProcessModeCommand    = "command"
	ProcessModeSupervised = "supervised"
)

// Config is the controller's process-level configuration.
type Config struct {
	// Control panel listener.
	ListenAddr string `json:"listen_addr"`
	ListenPort int    `json:"listen_port"`
	TLSEnabled bool   `json:"tls_enabled"`
	TLSCert    string `json:"tls_cert"`
	TLSKey     string `json:"tls_key"`

	// Controller state.
	DataDir string `json:"data_dir"`
	DBPath  string `json:"db_path"`

	// HAProxy integration.
	HAProxyBin     string `json:"haproxy_bin"`
	ConfigPath     string `json:"config_path"`
	ConfigDir      string `json:"config_dir"`
	ErrorPagesDir  string `json:"error_pages_dir"`
	CertsDir       string `json:"certs_dir"`
	MapsDir        string `json:"maps_dir"`
	BackupDir      string `json:"backup_dir"`
	RuntimeSocket  string `json:"runtime_socket"`
	MasterSocket   string `json:"master_socket"`
	PIDFile        string `json:"pid_file"`
	ReloadCommand  string `json:"reload_command"`
	RestartCommand string `json:"restart_command"`
	StatusCommand  string `json:"status_command"`

	// ProcessMode selects how HAProxy is driven:
	//   "command"    - shell out to systemctl or an equivalent (host install)
	//   "supervised" - run HAProxy as a child of this process (container)
	ProcessMode string `json:"process_mode"`

	// Security.
	SessionSecret  string   `json:"session_secret"`
	SessionTTLMins int      `json:"session_ttl_minutes"`
	TrustedProxies []string `json:"trusted_proxies"`
	IPAllowlist    []string `json:"ip_allowlist"`

	// Operational.
	LogLevel string `json:"log_level"`

	path string
	mu   sync.RWMutex

	// secretGenerated is set when finalize created a new session secret, so
	// Load knows to persist it.
	secretGenerated bool
}

// Default returns a Config populated with production-sane defaults.
func Default() *Config {
	return &Config{
		ListenAddr:     "0.0.0.0",
		ListenPort:     DefaultControlPort,
		TLSEnabled:     false,
		DataDir:        "/var/lib/haproxy-controller",
		DBPath:         "/var/lib/haproxy-controller/controller.db",
		HAProxyBin:     "/usr/local/sbin/haproxy",
		ConfigPath:     "/etc/haproxy/haproxy.cfg",
		ConfigDir:      "/etc/haproxy",
		ErrorPagesDir:  "/etc/haproxy/errors",
		CertsDir:       "/etc/haproxy/certs",
		MapsDir:        "/etc/haproxy/maps",
		BackupDir:      "/var/lib/haproxy-controller/backups",
		RuntimeSocket:  "/run/haproxy/admin.sock",
		MasterSocket:   "/run/haproxy/master.sock",
		PIDFile:        "/run/haproxy/haproxy.pid",
		ProcessMode:    ProcessModeCommand,
		ReloadCommand:  "systemctl reload haproxy-managed",
		RestartCommand: "systemctl restart haproxy-managed",
		StatusCommand:  "systemctl is-active haproxy-managed",
		SessionTTLMins: 720,
		LogLevel:       "info",
	}
}

// Load resolves configuration from all sources and ensures required
// directories exist. It is called once during startup.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("haproxy-controller", flag.ContinueOnError)

	var (
		configPath  = fs.String("config", "", "path to controller.json (default "+DefaultConfigPath+")")
		listenAddr  = fs.String("listen", "", "control panel bind address")
		listenPort  = fs.Int("port", 0, "control panel port")
		dataDir     = fs.String("data-dir", "", "controller state directory")
		haproxyBin  = fs.String("haproxy-bin", "", "path to the haproxy binary")
		cfgPath     = fs.String("haproxy-config", "", "path to the managed haproxy.cfg")
		processMode = fs.String("process-mode", "", `how to drive haproxy: "command" or "supervised"`)
		showVersion = fs.Bool("version", false, "print version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *showVersion {
		return nil, flag.ErrHelp
	}

	path := firstNonEmpty(*configPath, os.Getenv("HC_CONFIG"), DefaultConfigPath)

	cfg := Default()
	cfg.path = path

	if err := cfg.loadFile(path); err != nil {
		return nil, err
	}
	cfg.applyEnv()

	// Flags win over everything else.
	if *listenAddr != "" {
		cfg.ListenAddr = *listenAddr
	}
	if *listenPort != 0 {
		cfg.ListenPort = *listenPort
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
		cfg.DBPath = filepath.Join(*dataDir, "controller.db")
		cfg.BackupDir = filepath.Join(*dataDir, "backups")
	}
	if *haproxyBin != "" {
		cfg.HAProxyBin = *haproxyBin
	}
	if *cfgPath != "" {
		cfg.ConfigPath = *cfgPath
		cfg.ConfigDir = filepath.Dir(*cfgPath)
	}
	if *processMode != "" {
		cfg.ProcessMode = *processMode
	}

	if err := cfg.finalize(); err != nil {
		return nil, err
	}
	// Persist a freshly generated session secret so it survives restarts.
	// Without this, a container that never saves settings would get a new
	// secret on every boot, logging everyone out and orphaning the encrypted
	// assistant key.
	if cfg.secretGenerated {
		if err := cfg.Save(); err != nil {
			return nil, fmt.Errorf("persist generated session secret: %w", err)
		}
	}
	return cfg, nil
}

// loadFile merges an on-disk JSON config into cfg. A missing file is not an
// error: the controller boots on defaults and writes the file on first save.
func (c *Config) loadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, c); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

func (c *Config) applyEnv() {
	env := func(key string, dst *string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	env("HC_LISTEN_ADDR", &c.ListenAddr)
	env("HC_DATA_DIR", &c.DataDir)
	env("HC_DB_PATH", &c.DBPath)
	env("HC_HAPROXY_BIN", &c.HAProxyBin)
	env("HC_HAPROXY_CONFIG", &c.ConfigPath)
	env("HC_RUNTIME_SOCKET", &c.RuntimeSocket)
	env("HC_MASTER_SOCKET", &c.MasterSocket)
	env("HC_PID_FILE", &c.PIDFile)
	env("HC_PROCESS_MODE", &c.ProcessMode)
	env("HC_RELOAD_COMMAND", &c.ReloadCommand)
	env("HC_RESTART_COMMAND", &c.RestartCommand)
	env("HC_TLS_CERT", &c.TLSCert)
	env("HC_TLS_KEY", &c.TLSKey)
	env("HC_SESSION_SECRET", &c.SessionSecret)
	env("HC_LOG_LEVEL", &c.LogLevel)

	if v := os.Getenv("HC_LISTEN_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.ListenPort = p
		}
	}
	if v := os.Getenv("HC_TLS_ENABLED"); v != "" {
		c.TLSEnabled = isTruthy(v)
	}
}

// finalize derives dependent paths, generates a session secret on first boot
// and creates the directories the controller needs.
func (c *Config) finalize() error {
	if c.DataDir == "" {
		c.DataDir = Default().DataDir
	}
	if c.DBPath == "" {
		c.DBPath = filepath.Join(c.DataDir, "controller.db")
	}
	if c.BackupDir == "" {
		c.BackupDir = filepath.Join(c.DataDir, "backups")
	}
	if c.ConfigDir == "" {
		c.ConfigDir = filepath.Dir(c.ConfigPath)
	}
	if c.ListenPort <= 0 || c.ListenPort > 65535 {
		c.ListenPort = DefaultControlPort
	}
	if c.SessionTTLMins <= 0 {
		c.SessionTTLMins = 720
	}
	if c.ProcessMode != ProcessModeSupervised {
		c.ProcessMode = ProcessModeCommand
	}

	if c.SessionSecret == "" {
		secret, err := randomHex(32)
		if err != nil {
			return fmt.Errorf("generate session secret: %w", err)
		}
		c.SessionSecret = secret
		// The secret must be stable across restarts: it signs sessions and
		// encrypts the assistant's API key. If it is regenerated on every boot,
		// all sessions are invalidated and the stored key becomes unreadable.
		// So the freshly generated secret is persisted immediately.
		c.secretGenerated = true
	}

	dirs := []string{
		c.DataDir, c.BackupDir, c.ConfigDir,
		c.ErrorPagesDir, c.CertsDir, c.MapsDir,
		filepath.Dir(c.DBPath),
	}
	// HAProxy opens its stats and master sockets here, so the directory has
	// to exist before it starts.
	for _, sock := range []string{c.RuntimeSocket, c.MasterSocket, c.PIDFile} {
		if sock != "" {
			dirs = append(dirs, filepath.Dir(sock))
		}
	}
	for _, d := range dirs {
		if d == "" || d == "." {
			continue
		}
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("create directory %s: %w", d, err)
		}
	}
	// Certificates and keys must never be world readable.
	_ = os.Chmod(c.CertsDir, 0o700)
	return nil
}

// Path returns the config file location this Config was loaded from.
func (c *Config) Path() string { return c.path }

// SessionTTL returns the session lifetime as a duration.
func (c *Config) SessionTTL() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Duration(c.SessionTTLMins) * time.Minute
}

// Listener returns the current "host:port" the control panel should bind.
func (c *Config) Listener() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return fmt.Sprintf("%s:%d", c.ListenAddr, c.ListenPort)
}

// TLS reports whether TLS is enabled together with the key pair to use.
func (c *Config) TLS() (bool, string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.TLSEnabled && c.TLSCert != "" && c.TLSKey != "", c.TLSCert, c.TLSKey
}

// UpdateListener changes the control panel binding and persists it. The caller
// is responsible for asking the HTTP server to rebind.
func (c *Config) UpdateListener(addr string, port int, tlsEnabled bool, cert, key string) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("port %d is out of range", port)
	}
	if tlsEnabled && (cert == "" || key == "") {
		return fmt.Errorf("TLS requires both a certificate and a key")
	}
	c.mu.Lock()
	c.ListenAddr = addr
	c.ListenPort = port
	c.TLSEnabled = tlsEnabled
	c.TLSCert = cert
	c.TLSKey = key
	c.mu.Unlock()
	return c.Save()
}

// Save writes the configuration back to disk atomically with 0600 permissions,
// since it holds the session secret.
func (c *Config) Save() error {
	c.mu.RLock()
	data, err := json.MarshalIndent(c, "", "  ")
	path := c.path
	c.mu.RUnlock()
	if err != nil {
		return err
	}
	if path == "" {
		path = DefaultConfigPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
