package store

import (
	"strconv"
	"strings"
	"time"
)

// Roles recognised by the controller, ordered by privilege.
const (
	RoleAdmin    = "admin"    // full control, including users and settings
	RoleOperator = "operator" // may edit configuration and apply it
	RoleViewer   = "viewer"   // read-only
)

// User is a control panel account.
type User struct {
	ID             int64
	Username       string
	PasswordHash   string
	FullName       string
	Email          string
	Role           string
	IsActive       bool
	MustChangePw   bool
	FailedAttempts int
	LockedUntil    *time.Time
	LastLoginAt    *time.Time
	LastLoginIP    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CanEdit reports whether the user may change configuration.
func (u *User) CanEdit() bool { return u.Role == RoleAdmin || u.Role == RoleOperator }

// IsAdmin reports whether the user may manage accounts and controller settings.
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

// Session is a logged-in browser session.
type Session struct {
	ID         string
	UserID     int64
	TokenHash  string
	CSRFToken  string
	IP         string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// AuditEntry records a state-changing action.
type AuditEntry struct {
	ID        int64
	UserID    *int64
	Username  string
	Action    string
	Entity    string
	EntityID  string
	Detail    string
	IP        string
	CreatedAt time.Time
}

// GlobalConfig maps to HAProxy's `global` section.
type GlobalConfig struct {
	Maxconn                    int
	Nbthread                   int
	RunUser                    string
	RunGroup                   string
	Chroot                     string
	Daemon                     bool
	LogTargets                 string // one target per line
	StatsSocket                string
	StatsTimeout               string
	HardStopAfter              string
	SSLDefaultBindCiphers      string
	SSLDefaultBindCiphersuites string
	SSLDefaultBindOptions      string
	SSLDefaultServerCiphers    string
	SSLDefaultServerOptions    string
	TuneSSLDefaultDHParam      int
	SSLDHParamFile             string
	Extra                      string
}

// DefaultsConfig maps to a `defaults` section.
type DefaultsConfig struct {
	ID                   int64
	Name                 string
	Enabled              bool
	Mode                 string
	LogGlobal            bool
	Options              string // one bare option per line, e.g. "httplog"
	Retries              int
	Maxconn              int
	TimeoutConnect       string
	TimeoutClient        string
	TimeoutServer        string
	TimeoutHTTPRequest   string
	TimeoutHTTPKeepAlive string
	TimeoutQueue         string
	TimeoutCheck         string
	TimeoutTunnel        string
	Compression          string
	ErrorFilesRef        string
	Extra                string
	OrderIndex           int
}

// Frontend maps to a `frontend` section.
type Frontend struct {
	ID               int64
	Name             string
	Description      string
	Enabled          bool
	Mode             string
	DefaultBackendID *int64
	Maxconn          int
	OptionForwardFor bool
	OptionHTTPLog    bool
	OptionHTTPClose  bool
	ForceHTTPS       bool
	HSTSEnabled      bool
	HSTSMaxAge       int
	HSTSSubdomains   bool
	HSTSPreload      bool
	RateLimitEnabled bool
	RateLimitRPS     int
	RateLimitPeriod  string
	StatsEnabled     bool
	StatsURI         string
	StatsAuth        string
	HTTPErrorsRef    string
	LogSettings      string
	Extra            string
	OrderIndex       int
	CreatedAt        time.Time
	UpdatedAt        time.Time

	// Populated by the loader for rendering and the UI.
	Binds             []Bind
	ACLs              []ACL
	Rules             []Rule
	Domains           []Domain
	DefaultBackend    string
	ResolvedBackends  map[int64]string
	RenderedBindCount int
}

// Bind is one `bind` line inside a frontend.
type Bind struct {
	ID          int64
	FrontendID  int64
	Address     string
	Port        int
	Enabled     bool
	SSL         bool
	CertSource  string // "dir" (crt directory) or "cert" (managed certificate)
	CertRef     string
	ALPN        string
	AcceptProxy bool
	Transparent bool
	ExtraParams string
	OrderIndex  int
}

// Listen returns the bind address in HAProxy syntax.
func (b Bind) Listen() string {
	addr := strings.TrimSpace(b.Address)
	if addr == "" || addr == "*" {
		return ":" + itoa(b.Port)
	}
	if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
		// bare IPv6 literal
		return "[" + addr + "]:" + itoa(b.Port)
	}
	return addr + ":" + itoa(b.Port)
}

// Backend maps to a `backend` section.
type Backend struct {
	ID               int64
	Name             string
	Description      string
	Enabled          bool
	Mode             string
	Balance          string
	BalanceParam     string
	OptionForwardFor bool
	OptionHTTPClose  bool
	HTTPChkEnabled   bool
	HTTPChkMethod    string
	HTTPChkURI       string
	HTTPChkVersion   string
	HTTPChkHost      string
	CheckExpect      string
	TCPChkEnabled    bool
	CookieName       string
	CookieOptions    string
	StickEnabled     bool
	StickTable       string
	StickOn          string
	Retries          int
	TimeoutConnect   string
	TimeoutServer    string
	TimeoutCheck     string
	HTTPErrorsRef    string
	Extra            string
	OrderIndex       int
	CreatedAt        time.Time
	UpdatedAt        time.Time

	Servers []Server
	ACLs    []ACL
	Rules   []Rule
}

// Server is one `server` line inside a backend.
type Server struct {
	ID           int64
	BackendID    int64
	Name         string
	Address      string
	Port         int
	Enabled      bool
	Weight       int
	Maxconn      int
	CheckEnabled bool
	CheckInter   string
	CheckRise    int
	CheckFall    int
	SSL          bool
	SSLVerify    string
	SNI          string
	Backup       bool
	SendProxy    string
	CookieValue  string
	ExtraParams  string
	OrderIndex   int
}

// Domain is a host-based routing entry rendered into ACLs plus a
// use_backend or redirect rule on its frontend.
type Domain struct {
	ID           int64
	Hostname     string
	MatchType    string // exact | subdomain | wildcard | regex
	PathPrefix   string
	FrontendID   int64
	BackendID    *int64
	RedirectTo   string
	RedirectCode int
	ForceHTTPS   bool
	Enabled      bool
	OrderIndex   int
	CreatedAt    time.Time

	FrontendName string
	BackendName  string
}

// ACL is a named `acl` line.
type ACL struct {
	ID         int64
	Scope      string // frontend | backend
	OwnerID    int64
	Name       string
	Expression string
	Enabled    bool
	OrderIndex int
}

// Rule is any ordered directive with an optional condition, such as
// `http-request deny if bad-agent`.
type Rule struct {
	ID         int64
	Scope      string
	OwnerID    int64
	Directive  string // http-request, http-response, redirect, use_backend, ...
	Argument   string
	Condition  string // "if ..." / "unless ..."; stored without the keyword
	Enabled    bool
	OrderIndex int
}

// Certificate is a TLS key pair managed by the controller. The PEM material
// lives in the database and is written to the certs directory on deploy.
type Certificate struct {
	ID          int64
	Name        string
	FileName    string
	Domains     string
	Subject     string
	Issuer      string
	Serial      string
	Fingerprint string
	NotBefore   string
	NotAfter    string
	CertPEM     string
	KeyPEM      string
	ChainPEM    string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ErrorPage is a custom response body served for a status code. It is written
// out in raw HTTP response format, which is what `errorfile` expects.
type ErrorPage struct {
	ID          int64
	Name        string
	GroupName   string
	StatusCode  int
	ContentType string
	Headers     string // one "Name: value" per line
	Body        string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Snippet is operator-supplied configuration that the structured model does
// not cover: whole sections (resolvers, peers, userlist, cache, ...) or
// extra lines appended to global/defaults.
type Snippet struct {
	ID          int64
	Name        string
	SectionType string // resolvers | peers | userlist | cache | ring | mailers | listen | program | raw
	SectionArg  string
	Body        string
	Enabled     bool
	OrderIndex  int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ConfigVersion is a rendered haproxy.cfg retained for diffing and rollback.
type ConfigVersion struct {
	ID        int64
	Content   string
	Checksum  string
	Status    string // applied | failed | rolled_back
	Comment   string
	CreatedBy string
	Error     string
	CreatedAt time.Time
}

func itoa(i int) string { return strconv.Itoa(i) }
