package hap

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

// aclSanitize reduces a hostname to characters valid in an ACL name.
var aclSanitize = regexp.MustCompile(`[^A-Za-z0-9]+`)

// aclName derives a deterministic, collision-resistant ACL name for a route.
func aclName(prefix string, d store.Domain) string {
	base := aclSanitize.ReplaceAllString(strings.ToLower(d.Hostname), "_")
	base = strings.Trim(base, "_")
	if base == "" {
		base = "route"
	}
	// The row id keeps two routes for the same host distinct.
	return fmt.Sprintf("%s_%s_%d", prefix, base, d.ID)
}

// hostExpr extracts the request Host header, lowercased and without any port,
// so exact matches behave the way an operator expects.
const hostExpr = "req.hdr(host),lower,field(1,:)"

func (r *Renderer) renderFrontend(
	ctx context.Context,
	b *buf,
	f *store.Frontend,
	backends map[int64]*store.Backend,
	certs map[string]*store.Certificate,
) ([]string, error) {
	var warnings []string

	binds, err := r.st.ListBinds(ctx, f.ID)
	if err != nil {
		return nil, fmt.Errorf("load binds for frontend %s: %w", f.Name, err)
	}
	acls, err := r.st.ListACLs(ctx, "frontend", f.ID)
	if err != nil {
		return nil, fmt.Errorf("load ACLs for frontend %s: %w", f.Name, err)
	}
	rules, err := r.st.ListRules(ctx, "frontend", f.ID)
	if err != nil {
		return nil, fmt.Errorf("load rules for frontend %s: %w", f.Name, err)
	}
	domains, err := r.st.ListDomainsByFrontend(ctx, f.ID)
	if err != nil {
		return nil, fmt.Errorf("load routes for frontend %s: %w", f.Name, err)
	}

	if f.Description != "" {
		b.line("# %s", strings.ReplaceAll(f.Description, "\n", " "))
	}
	b.line("frontend %s", f.Name)
	b.kv("mode", f.Mode)

	// ---- binds
	active := 0
	for _, bd := range binds {
		if !bd.Enabled {
			continue
		}
		line, w := r.bindLine(f, bd, certs)
		warnings = append(warnings, w...)
		b.line("    %s", line)
		active++
	}
	if active == 0 {
		warnings = append(warnings,
			fmt.Sprintf("Frontend %q has no enabled bind, so it will not listen on any port.", f.Name))
	}

	b.kvInt("maxconn", f.Maxconn)
	if f.Mode == "http" && f.OptionHTTPLog {
		b.line("    option httplog")
	}
	if f.Mode == "http" && f.OptionForwardFor {
		b.line("    option forwardfor")
	}
	if f.OptionHTTPClose {
		b.line("    option http-server-close")
	}
	for _, l := range store.SplitLines(f.LogSettings) {
		b.kv("log", l)
	}
	b.kv("errorfiles", f.HTTPErrorsRef)

	// ---- rate limiting: one shared stick-table keyed on source address
	if f.RateLimitEnabled && f.RateLimitRPS > 0 {
		period := f.RateLimitPeriod
		if period == "" {
			period = "10s"
		}
		b.line("    stick-table type ip size 100k expire %s store http_req_rate(%s)", period, period)
		b.line("    http-request track-sc0 src")
		b.line("    http-request deny deny_status 429 if { sc_http_req_rate(0) gt %d }", f.RateLimitRPS)
	}

	// ---- HSTS, emitted only on TLS connections
	if f.Mode == "http" && f.HSTSEnabled {
		v := fmt.Sprintf("max-age=%d", f.HSTSMaxAge)
		if f.HSTSSubdomains {
			v += "; includeSubDomains"
		}
		if f.HSTSPreload {
			v += "; preload"
		}
		// http-after-response also covers responses HAProxy generates itself
		// (error pages, redirects), which http-response rules never reach.
		b.line("    http-after-response set-header Strict-Transport-Security \"%s\" if { ssl_fc }", v)
	}

	// ---- operator ACLs
	for _, a := range acls {
		if a.Enabled {
			b.line("    acl %s %s", a.Name, a.Expression)
		}
	}

	// ---- route ACLs derived from the Domains page
	type route struct {
		domain store.Domain
		host   string
		path   string
	}
	var routes []route
	for _, d := range domains {
		if !d.Enabled {
			continue
		}
		host := aclName("host", d)
		for _, line := range hostACLLines(host, d) {
			b.line("    %s", line)
		}
		path := ""
		if d.PathPrefix != "" {
			path = aclName("path", d)
			b.line("    acl %s path_beg %s", path, d.PathPrefix)
		}
		routes = append(routes, route{domain: d, host: host, path: path})
	}

	// ---- redirects, which must precede routing decisions
	if f.Mode == "http" && f.ForceHTTPS {
		b.line("    http-request redirect scheme https code 301 unless { ssl_fc }")
	}
	for _, rt := range routes {
		cond := rt.host
		if rt.path != "" {
			cond += " " + rt.path
		}
		if rt.domain.ForceHTTPS && !f.ForceHTTPS && f.Mode == "http" {
			b.line("    http-request redirect scheme https code 301 if %s !{ ssl_fc }", cond)
		}
		if to := strings.TrimSpace(rt.domain.RedirectTo); to != "" {
			code := rt.domain.RedirectCode
			if code == 0 {
				code = 301
			}
			b.line("    http-request redirect location %s code %d if %s", quoteIfNeeded(to), code, cond)
		}
	}

	// ---- operator rules, in explicit order
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		b.line("    %s", renderRule(rule))
	}

	// ---- backend selection
	for _, rt := range routes {
		if rt.domain.BackendID == nil {
			continue
		}
		bk, ok := backends[*rt.domain.BackendID]
		if !ok {
			warnings = append(warnings,
				fmt.Sprintf("Route %q points at a backend that no longer exists and was skipped.", rt.domain.Hostname))
			continue
		}
		if !bk.Enabled {
			warnings = append(warnings,
				fmt.Sprintf("Route %q points at disabled backend %q and was skipped.", rt.domain.Hostname, bk.Name))
			continue
		}
		cond := rt.host
		if rt.path != "" {
			cond += " " + rt.path
		}
		b.line("    use_backend %s if %s", bk.Name, cond)
	}

	if f.DefaultBackendID != nil {
		if bk, ok := backends[*f.DefaultBackendID]; ok {
			if bk.Enabled {
				b.line("    default_backend %s", bk.Name)
			} else {
				warnings = append(warnings,
					fmt.Sprintf("Frontend %q has disabled backend %q as its default; no default_backend was written.", f.Name, bk.Name))
			}
		}
	} else if len(routes) == 0 {
		warnings = append(warnings,
			fmt.Sprintf("Frontend %q has no default backend and no routes; every request will fail.", f.Name))
	}

	// ---- built-in stats page
	if f.StatsEnabled {
		b.line("    stats enable")
		b.kv("stats uri", f.StatsURI)
		b.line("    stats refresh 10s")
		b.line("    stats show-legends")
		b.kv("stats auth", f.StatsAuth)
		if strings.TrimSpace(f.StatsAuth) == "" {
			warnings = append(warnings,
				fmt.Sprintf("Frontend %q exposes the stats page without authentication.", f.Name))
		}
	}

	if strings.TrimSpace(f.Extra) != "" {
		b.raw(f.Extra)
	}
	b.blank()
	return warnings, nil
}

// hostACLLines builds the acl line(s) for one route. Repeating the same ACL
// name is how HAProxy expresses "any of these patterns".
func hostACLLines(name string, d store.Domain) []string {
	host := strings.ToLower(strings.TrimSpace(d.Hostname))
	switch d.MatchType {
	case "subdomain":
		bare := strings.TrimPrefix(host, "*.")
		return []string{
			fmt.Sprintf("acl %s %s -m str %s", name, hostExpr, bare),
			fmt.Sprintf("acl %s %s -m end .%s", name, hostExpr, bare),
		}
	case "wildcard":
		bare := strings.TrimPrefix(host, "*.")
		return []string{fmt.Sprintf("acl %s %s -m end .%s", name, hostExpr, bare)}
	case "regex":
		return []string{fmt.Sprintf("acl %s %s -m reg %s", name, hostExpr, host)}
	default: // exact
		return []string{fmt.Sprintf("acl %s %s -m str %s", name, hostExpr, host)}
	}
}

// bindLine assembles one `bind` directive.
func (r *Renderer) bindLine(f *store.Frontend, bd store.Bind, certs map[string]*store.Certificate) (string, []string) {
	var warnings []string
	var sb strings.Builder
	sb.WriteString("bind ")
	sb.WriteString(bd.Listen())

	if bd.AcceptProxy {
		sb.WriteString(" accept-proxy")
	}
	if bd.Transparent {
		sb.WriteString(" transparent")
	}

	if bd.SSL {
		sb.WriteString(" ssl")
		switch bd.CertSource {
		case "cert":
			cert, ok := certs[bd.CertRef]
			if !ok {
				warnings = append(warnings, fmt.Sprintf(
					"Frontend %q binds %s with certificate %q, which no longer exists; falling back to the certificate directory.",
					f.Name, bd.Listen(), bd.CertRef))
				sb.WriteString(" crt " + r.paths.CertsDir + "/")
			} else {
				sb.WriteString(" crt " + filepath.Join(r.paths.CertsDir, cert.FileName))
			}
		default:
			// Whole-directory mode: HAProxy picks the right cert by SNI.
			sb.WriteString(" crt " + strings.TrimSuffix(r.paths.CertsDir, "/") + "/")
		}
		if alpn := strings.TrimSpace(bd.ALPN); alpn != "" {
			sb.WriteString(" alpn " + alpn)
		}
	}

	if extra := strings.TrimSpace(bd.ExtraParams); extra != "" {
		sb.WriteString(" " + extra)
	}
	return sb.String(), warnings
}

// renderRule assembles one ordered directive line.
func renderRule(r store.Rule) string {
	parts := []string{r.Directive}
	if a := strings.TrimSpace(r.Argument); a != "" {
		parts = append(parts, a)
	}
	if c := strings.TrimSpace(r.Condition); c != "" {
		parts = append(parts, c)
	}
	return strings.Join(parts, " ")
}

// quoteIfNeeded wraps a value in double quotes when it contains a space, which
// HAProxy would otherwise read as the start of the next argument.
func quoteIfNeeded(v string) string {
	if strings.ContainsAny(v, " \t") && !strings.HasPrefix(v, `"`) {
		return strconv.Quote(v)
	}
	return v
}

func (r *Renderer) renderBackend(ctx context.Context, b *buf, bk *store.Backend) ([]string, error) {
	var warnings []string

	servers, err := r.st.ListServers(ctx, bk.ID)
	if err != nil {
		return nil, fmt.Errorf("load servers for backend %s: %w", bk.Name, err)
	}
	acls, err := r.st.ListACLs(ctx, "backend", bk.ID)
	if err != nil {
		return nil, fmt.Errorf("load ACLs for backend %s: %w", bk.Name, err)
	}
	rules, err := r.st.ListRules(ctx, "backend", bk.ID)
	if err != nil {
		return nil, fmt.Errorf("load rules for backend %s: %w", bk.Name, err)
	}

	if bk.Description != "" {
		b.line("# %s", strings.ReplaceAll(bk.Description, "\n", " "))
	}
	b.line("backend %s", bk.Name)
	b.kv("mode", bk.Mode)

	balance := bk.Balance
	if bk.BalanceParam != "" {
		balance += " " + bk.BalanceParam
	}
	b.kv("balance", balance)

	if bk.Mode == "http" && bk.OptionForwardFor {
		b.line("    option forwardfor")
	}
	if bk.OptionHTTPClose {
		b.line("    option http-server-close")
	}
	b.kvInt("retries", bk.Retries)
	b.kv("timeout connect", bk.TimeoutConnect)
	b.kv("timeout server", bk.TimeoutServer)
	b.kv("timeout check", bk.TimeoutCheck)
	b.kv("errorfiles", bk.HTTPErrorsRef)

	// ---- health checking, using the modern http-check syntax
	if bk.HTTPChkEnabled && bk.Mode == "http" {
		b.line("    option httpchk")
		send := fmt.Sprintf("http-check send meth %s uri %s",
			orDefault(bk.HTTPChkMethod, "GET"), orDefault(bk.HTTPChkURI, "/"))
		if v := strings.TrimSpace(bk.HTTPChkVersion); v != "" {
			send += " ver " + v
			// HTTP/1.1 checks must carry a Host header or servers may 400.
			host := strings.TrimSpace(bk.HTTPChkHost)
			if host == "" && v == "HTTP/1.1" {
				host = "localhost"
			}
			if host != "" {
				send += " hdr Host " + host
			}
		}
		b.line("    %s", send)
		if e := strings.TrimSpace(bk.CheckExpect); e != "" {
			b.line("    http-check expect %s", e)
		}
	}
	if bk.TCPChkEnabled {
		b.line("    option tcp-check")
	}

	// ---- cookie-based session persistence
	if c := strings.TrimSpace(bk.CookieName); c != "" {
		b.line("    cookie %s %s", c, strings.TrimSpace(bk.CookieOptions))
	}
	if bk.StickEnabled {
		b.kv("stick-table", bk.StickTable)
		b.kv("stick on", bk.StickOn)
	}

	for _, a := range acls {
		if a.Enabled {
			b.line("    acl %s %s", a.Name, a.Expression)
		}
	}
	for _, rule := range rules {
		if rule.Enabled {
			b.line("    %s", renderRule(rule))
		}
	}

	// ---- servers
	enabled := 0
	for _, sv := range servers {
		if !sv.Enabled {
			continue
		}
		b.line("    %s", serverLine(bk, sv))
		enabled++
	}
	if enabled == 0 {
		warnings = append(warnings,
			fmt.Sprintf("Backend %q has no enabled server; requests routed to it will return 503.", bk.Name))
	}
	if strings.TrimSpace(bk.CookieName) != "" {
		for _, sv := range servers {
			if sv.Enabled && strings.TrimSpace(sv.CookieValue) == "" {
				warnings = append(warnings, fmt.Sprintf(
					"Backend %q uses cookie persistence but server %q has no cookie value, so sessions will not stick to it.",
					bk.Name, sv.Name))
			}
		}
	}

	if strings.TrimSpace(bk.Extra) != "" {
		b.raw(bk.Extra)
	}
	b.blank()
	return warnings, nil
}

// serverLine assembles one `server` directive.
func serverLine(bk *store.Backend, sv store.Server) string {
	addr := sv.Address
	if sv.Port > 0 {
		if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
			addr = "[" + addr + "]"
		}
		addr = fmt.Sprintf("%s:%d", addr, sv.Port)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "server %s %s", sv.Name, addr)

	if sv.CheckEnabled {
		sb.WriteString(" check")
		if v := strings.TrimSpace(sv.CheckInter); v != "" {
			sb.WriteString(" inter " + v)
		}
		if sv.CheckRise > 0 {
			fmt.Fprintf(&sb, " rise %d", sv.CheckRise)
		}
		if sv.CheckFall > 0 {
			fmt.Fprintf(&sb, " fall %d", sv.CheckFall)
		}
	}
	if sv.Weight >= 0 && sv.Weight != 100 {
		fmt.Fprintf(&sb, " weight %d", sv.Weight)
	}
	if sv.Maxconn > 0 {
		fmt.Fprintf(&sb, " maxconn %d", sv.Maxconn)
	}
	if sv.SSL {
		sb.WriteString(" ssl")
		verify := strings.TrimSpace(sv.SSLVerify)
		if verify == "" {
			verify = "none"
		}
		sb.WriteString(" verify " + verify)
		if s := strings.TrimSpace(sv.SNI); s != "" {
			sb.WriteString(" sni " + s)
		}
	}
	if sv.Backup {
		sb.WriteString(" backup")
	}
	if p := strings.TrimSpace(sv.SendProxy); p != "" {
		sb.WriteString(" " + p)
	}
	// A cookie value is only meaningful when the backend sets a cookie name.
	if c := strings.TrimSpace(sv.CookieValue); c != "" && strings.TrimSpace(bk.CookieName) != "" {
		sb.WriteString(" cookie " + c)
	}
	if e := strings.TrimSpace(sv.ExtraParams); e != "" {
		sb.WriteString(" " + e)
	}
	return sb.String()
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
