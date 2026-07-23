package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// Brand constants shown across the panel and the generated error pages.
const (
	BrandName = "Ebdaa.me"
	BrandURL  = "https://ebdaa.me"
)

// BootstrapOptions tunes the seeded defaults for the environment the
// controller was started in.
type BootstrapOptions struct {
	// LogTarget is the default `log` line for the global section. A container
	// has no syslog socket, so it must log to stdout instead.
	LogTarget string
}

// Bootstrap seeds the rows the controller needs to render a working
// configuration on a fresh database. It is idempotent.
func (s *Store) Bootstrap(ctx context.Context, opts BootstrapOptions) error {
	if err := s.seedGlobal(ctx, opts); err != nil {
		return fmt.Errorf("seed global: %w", err)
	}
	if err := s.seedDefaults(ctx); err != nil {
		return fmt.Errorf("seed defaults: %w", err)
	}
	if err := s.seedErrorPages(ctx); err != nil {
		return fmt.Errorf("seed error pages: %w", err)
	}
	return nil
}

func (s *Store) seedGlobal(ctx context.Context, opts BootstrapOptions) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	if _, err := s.db.ExecContext(c, "INSERT OR IGNORE INTO global_config (id) VALUES (1)"); err != nil {
		return err
	}
	// Only steer the log target on a fresh row, so an operator's own choice
	// is never overwritten on a later start.
	if strings.TrimSpace(opts.LogTarget) != "" {
		_, err := s.db.ExecContext(c, `
			UPDATE global_config SET log_targets = ?
			WHERE id = 1 AND log_targets = '/dev/log local0'`, opts.LogTarget)
		return err
	}
	return nil
}

func (s *Store) seedDefaults(ctx context.Context) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	var n int
	if err := s.db.QueryRowContext(c, "SELECT COUNT(*) FROM defaults_config").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.ExecContext(c, `
		INSERT INTO defaults_config (name, enabled, mode, log_global, options, order_index)
		VALUES ('', 1, 'http', 1, ?, 0)`,
		strings.Join([]string{"httplog", "dontlognull", "redispatch", "http-server-close"}, "\n"))
	return err
}

// seedErrorPages installs a branded page for each status code HAProxy serves
// by default, so a fresh install never shows a bare HAProxy error.
func (s *Store) seedErrorPages(ctx context.Context) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()

	var n int
	if err := s.db.QueryRowContext(c, "SELECT COUNT(*) FROM error_pages").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	// HAProxy only emits these codes itself; seeding exactly this set keeps
	// the default `http-errors default` section complete and minimal.
	seeded := []struct {
		code    int
		title   string
		message string
	}{
		{400, "Bad Request", "The request could not be understood by the server."},
		{403, "Forbidden", "You do not have permission to access this resource."},
		{404, "Not Found", "The page you are looking for could not be found."},
		{408, "Request Timeout", "The server timed out waiting for the request."},
		{500, "Internal Server Error", "The server encountered an unexpected condition."},
		{502, "Bad Gateway", "The upstream server returned an invalid response."},
		{503, "Service Unavailable", "No server is available to handle this request right now."},
		{504, "Gateway Timeout", "The upstream server did not respond in time."},
	}

	tx, err := s.db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, p := range seeded {
		_, err := tx.ExecContext(c, `
			INSERT INTO error_pages (name, group_name, status_code, content_type, body, enabled)
			VALUES (?, 'default', ?, 'text/html; charset=utf-8', ?, 1)`,
			fmt.Sprintf("default-%d", p.code), p.code,
			DefaultErrorBody(p.code, p.title, p.message))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DefaultErrorBody renders the stock branded error page markup. It is
// self-contained: no external assets, so it renders even when every backend
// is down.
func DefaultErrorBody(code int, title, message string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%d %s</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: #f6f7f9; color: #1c2430; padding: 24px;
  }
  .card {
    background: #fff; border: 1px solid #e3e7ec; border-radius: 14px; padding: 48px 40px;
    max-width: 540px; width: 100%%; text-align: center;
    box-shadow: 0 1px 2px rgba(16,24,40,.04), 0 8px 24px rgba(16,24,40,.06);
  }
  .code { font-size: 60px; font-weight: 700; letter-spacing: -.02em; margin: 0; color: #0f172a; }
  h1 { font-size: 20px; font-weight: 600; margin: 12px 0 8px; }
  p  { margin: 0; color: #5b6675; line-height: 1.6; font-size: 15px; }
  .footer { margin-top: 32px; padding-top: 20px; border-top: 1px solid #eef1f4; font-size: 13px; color: #8a94a3; }
  .footer a { color: #2563eb; text-decoration: none; }
  .footer a:hover { text-decoration: underline; }
  @media (prefers-color-scheme: dark) {
    body { background: #0d1117; color: #e6edf3; }
    .card { background: #161b22; border-color: #2b323b; box-shadow: none; }
    .code { color: #e6edf3; }
    p { color: #9aa5b1; }
    .footer { border-top-color: #2b323b; color: #7d8792; }
    .footer a { color: #58a6ff; }
  }
</style>
</head>
<body>
  <div class="card">
    <p class="code">%d</p>
    <h1>%s</h1>
    <p>%s</p>
    <div class="footer">Powered by <a href="%s" rel="noopener">%s</a></div>
  </div>
</body>
</html>
`, code, title, code, title, message, BrandURL, BrandName)
}

// EnsureAdmin creates the initial administrator when the user table is empty
// and returns the generated password. An empty password means an account
// already existed and nothing was created.
func (s *Store) EnsureAdmin(ctx context.Context, username string) (string, error) {
	n, err := s.CountUsers(ctx)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "", nil
	}
	password, err := GeneratePassword(20)
	if err != nil {
		return "", err
	}
	_, err = s.CreateUser(ctx, &User{
		Username:     username,
		FullName:     "Administrator",
		Role:         RoleAdmin,
		IsActive:     true,
		MustChangePw: true,
	}, password)
	if err != nil {
		return "", err
	}
	return password, nil
}

// passwordAlphabet omits characters that are easy to confuse when a generated
// password is read off a terminal.
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789@#%+=?"

// GeneratePassword returns a cryptographically random password. It always
// contains at least one letter and one digit so it satisfies the policy.
func GeneratePassword(n int) (string, error) {
	if n < 12 {
		n = 12
	}
	for attempt := 0; attempt < 20; attempt++ {
		var b strings.Builder
		for i := 0; i < n; i++ {
			idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(passwordAlphabet))))
			if err != nil {
				return "", err
			}
			b.WriteByte(passwordAlphabet[idx.Int64()])
		}
		pw := b.String()
		if ValidatePassword(pw) == nil {
			return pw, nil
		}
	}
	return "", fmt.Errorf("could not generate a compliant password")
}
