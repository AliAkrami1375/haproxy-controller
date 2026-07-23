package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

// toolHandler executes a tool call and returns a result string that is fed
// back to the model. A returned error is also reported to the model so it can
// correct itself, rather than aborting the run.
type toolHandler func(ctx context.Context, env *Env, args map[string]any) (string, error)

// registeredTool bundles a tool's schema with its handler and a flag for
// whether it changes state (used to label steps in the UI).
type registeredTool struct {
	def     Tool
	handler toolHandler
	mutates bool
}

// tool builds a registeredTool from its pieces.
func tool(name, desc string, params map[string]any, mutates bool, h toolHandler) registeredTool {
	return registeredTool{
		def: Tool{Type: "function", Function: FunctionDefinition{
			Name: name, Description: desc, Parameters: params,
		}},
		handler: h,
		mutates: mutates,
	}
}

// Registry is the set of tools the agent may call.
type Registry struct {
	tools map[string]registeredTool
	order []string
}

// Definitions returns the tool schemas to send to the model.
func (r *Registry) Definitions() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name].def)
	}
	return out
}

// Mutates reports whether a tool changes state.
func (r *Registry) Mutates(name string) bool {
	t, ok := r.tools[name]
	return ok && t.mutates
}

// Call executes a named tool.
func (r *Registry) Call(ctx context.Context, env *Env, name string, rawArgs string) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("no such tool %q", name)
	}
	args := map[string]any{}
	if s := strings.TrimSpace(rawArgs); s != "" && s != "null" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			return "", fmt.Errorf("could not parse arguments as JSON: %v", err)
		}
	}
	return t.handler(ctx, env, args)
}

// NewRegistry builds the full tool set the HAProxy agent uses.
func NewRegistry() *Registry {
	r := &Registry{tools: map[string]registeredTool{}}
	add := func(t registeredTool) {
		r.tools[t.def.Function.Name] = t
		r.order = append(r.order, t.def.Function.Name)
	}

	// ---------------------------------------------------------- read/context
	add(tool("get_overview",
		"Summarise the current configuration: counts and the names of every frontend, backend, domain, certificate and error-page group. Call this first to understand what already exists.",
		obj(map[string]any{}), false, toolOverview))

	add(tool("list_backends",
		"List every backend with its servers and health-check settings.",
		obj(map[string]any{}), false, toolListBackends))

	add(tool("list_frontends",
		"List every frontend with its listeners (binds) and default backend.",
		obj(map[string]any{}), false, toolListFrontends))

	add(tool("validate_config",
		"Render the full HAProxy configuration from the current state and validate it with HAProxy itself. Returns whether it is valid, HAProxy's diagnostics, and any warnings. Call this after making changes, and repair anything it reports before finishing.",
		obj(map[string]any{}), false, toolValidate))

	// ------------------------------------------------------------- backends
	add(tool("create_backend",
		"Create a backend (a pool of servers). Choose a load balancing algorithm and, for HTTP, optionally enable an HTTP health check.",
		obj(map[string]any{
			"name":         str("Unique backend name (letters, digits, dot, underscore, hyphen)."),
			"mode":         enum("Proxy mode.", "http", "tcp"),
			"balance":      enum("Load balancing algorithm.", "roundrobin", "leastconn", "static-rr", "source", "uri", "random"),
			"description":  str("Optional human description."),
			"health_check": boolean("Enable an HTTP health check (http mode only)."),
			"check_uri":    str("Health check path, e.g. /healthz. Defaults to /."),
			"check_host":   str("Host header for the health check, e.g. app.example.com."),
			"check_expect": str("Expected result, e.g. 'status 200'. Defaults to status 200."),
			"cookie_name":  str("Optional cookie name to enable cookie-based session persistence."),
		}, "name", "mode", "balance"), true, toolCreateBackend))

	add(tool("add_server",
		"Add a server to an existing backend.",
		obj(map[string]any{
			"backend": str("Name of the backend to add the server to."),
			"name":    str("Unique server name within the backend."),
			"address": str("Server address (IP or hostname)."),
			"port":    integer("Server port (1-65535)."),
			"weight":  integer("Relative weight (0-256). Defaults to 100."),
			"check":   boolean("Enable health checking for this server. Defaults to true."),
			"backup":  boolean("Mark as a backup server, used only when others are down."),
			"ssl":     boolean("Connect to the server over TLS."),
		}, "backend", "name", "address", "port"), true, toolAddServer))

	// ------------------------------------------------------------ frontends
	add(tool("create_frontend",
		"Create a frontend (an entry point that accepts traffic). Add listeners with add_bind afterwards.",
		obj(map[string]any{
			"name":            str("Unique frontend name."),
			"mode":            enum("Proxy mode.", "http", "tcp"),
			"default_backend": str("Name of the backend to send unmatched traffic to. Optional."),
			"force_https":     boolean("Redirect every plain HTTP request to HTTPS."),
			"enable_hsts":     boolean("Send a Strict-Transport-Security header on TLS connections."),
			"description":     str("Optional human description."),
		}, "name", "mode"), true, toolCreateFrontend))

	add(tool("add_bind",
		"Add a listener (bind) to a frontend: an address and port, optionally with TLS.",
		obj(map[string]any{
			"frontend":    str("Name of the frontend."),
			"address":     str("Bind address. Use '*' for every interface. Defaults to '*'."),
			"port":        integer("Port to listen on (1-65535)."),
			"ssl":         boolean("Terminate TLS on this listener."),
			"certificate": str("For TLS: the name of a managed certificate, or leave empty to use the certificate directory (SNI selects automatically)."),
		}, "frontend", "port"), true, toolAddBind))

	add(tool("set_default_backend",
		"Set (or change) the default backend of a frontend.",
		obj(map[string]any{
			"frontend": str("Frontend name."),
			"backend":  str("Backend name."),
		}, "frontend", "backend"), true, toolSetDefaultBackend))

	// -------------------------------------------------------------- routing
	add(tool("add_route",
		"Route a hostname on a frontend to a backend, or redirect it. Creates the ACL and rule for you.",
		obj(map[string]any{
			"frontend":    str("Frontend the traffic arrives on."),
			"hostname":    str("Hostname to match, e.g. app.example.com or *.example.com."),
			"match_type":  enum("How to match the hostname.", "exact", "subdomain", "wildcard", "regex"),
			"backend":     str("Backend to route matching traffic to. Provide this or redirect_to."),
			"path_prefix": str("Optional path prefix to narrow the match, e.g. /api."),
			"redirect_to": str("Redirect matching requests here instead of routing to a backend."),
			"force_https": boolean("Redirect matching plain HTTP requests to HTTPS."),
		}, "frontend", "hostname"), true, toolAddRoute))

	add(tool("add_acl",
		"Add a named ACL (condition) to a frontend or backend for use by rules.",
		obj(map[string]any{
			"scope":      enum("Where the ACL lives.", "frontend", "backend"),
			"owner":      str("Name of the frontend or backend."),
			"name":       str("ACL name."),
			"expression": str("ACL expression, e.g. 'path_beg /api' or 'src 10.0.0.0/8'."),
		}, "scope", "owner", "name", "expression"), true, toolAddACL))

	add(tool("add_rule",
		"Add an ordered rule to a frontend or backend, e.g. an http-request directive with a condition.",
		obj(map[string]any{
			"scope":     enum("Where the rule lives.", "frontend", "backend"),
			"owner":     str("Name of the frontend or backend."),
			"directive": str("Directive, e.g. 'http-request', 'http-response', 'redirect'."),
			"argument":  str("Directive argument, e.g. 'deny deny_status 403'."),
			"condition": str("Optional condition, must start with 'if ' or 'unless ', e.g. 'if is_api'."),
		}, "scope", "owner", "directive"), true, toolAddRule))

	// --------------------------------------------------------- error pages
	add(tool("create_error_page",
		"Create a branded error page for an HTTP status code, using the built-in professional template.",
		obj(map[string]any{
			"group":       str("Error page group, e.g. 'default' or 'maintenance'. Defaults to 'default'."),
			"status_code": integer("HTTP status code, e.g. 404, 500, 503."),
			"title":       str("Page heading, e.g. 'Service Unavailable'."),
			"message":     str("Explanatory sentence shown to the visitor."),
		}, "status_code", "title", "message"), true, toolCreateErrorPage))

	// -------------------------------------------------------------- snippet
	add(tool("create_snippet",
		"Add a raw configuration snippet for anything the structured tools do not cover (resolvers, peers, userlist, a standalone listen section, etc.). The body is validated before it can be applied.",
		obj(map[string]any{
			"name":         str("Snippet name."),
			"section_type": enum("Section kind.", "resolvers", "userlist", "peers", "cache", "listen", "global-extra", "defaults-extra", "raw"),
			"section_arg":  str("Section name, required for named sections like resolvers or listen."),
			"body":         str("The section body, without the header line. One directive per line."),
		}, "name", "section_type", "body"), true, toolCreateSnippet))

	// --------------------------------------------------------------- delete
	add(tool("delete_backend",
		"Delete a backend by name. Fails if a frontend or route still uses it.",
		obj(map[string]any{"name": str("Backend name.")}, "name"), true, toolDeleteBackend))

	add(tool("delete_frontend",
		"Delete a frontend by name, including its listeners and routes.",
		obj(map[string]any{"name": str("Frontend name.")}, "name"), true, toolDeleteFrontend))

	return r
}

// ------------------------------------------------------------ read handlers

func toolOverview(ctx context.Context, env *Env, _ map[string]any) (string, error) {
	frontends, _ := env.Store.ListFrontends(ctx)
	backends, _ := env.Store.ListBackends(ctx)
	domains, _ := env.Store.ListDomains(ctx)
	certs, _ := env.Store.ListCertificates(ctx)
	groups, _ := env.Store.ErrorPageGroups(ctx)

	type ov struct {
		Frontends    []string `json:"frontends"`
		Backends     []string `json:"backends"`
		Domains      []string `json:"domains"`
		Certificates []string `json:"certificates"`
		ErrorGroups  []string `json:"error_page_groups"`
	}
	var o ov
	for _, f := range frontends {
		state := ""
		if !f.Enabled {
			state = " (disabled)"
		}
		o.Frontends = append(o.Frontends, f.Name+state)
	}
	for _, b := range backends {
		o.Backends = append(o.Backends, b.Name)
	}
	for _, d := range domains {
		o.Domains = append(o.Domains, d.Hostname)
	}
	for _, c := range certs {
		o.Certificates = append(o.Certificates, c.Name)
	}
	o.ErrorGroups = groups
	return toJSON(o), nil
}

func toolListBackends(ctx context.Context, env *Env, _ map[string]any) (string, error) {
	backends, err := env.Store.ListBackends(ctx)
	if err != nil {
		return "", err
	}
	type sv struct {
		Name, Address string
		Port, Weight  int
		Backup        bool
	}
	type bk struct {
		Name, Mode, Balance string
		HealthCheck         bool
		Servers             []sv
	}
	var out []bk
	for _, b := range backends {
		servers, _ := env.Store.ListServers(ctx, b.ID)
		entry := bk{Name: b.Name, Mode: b.Mode, Balance: b.Balance, HealthCheck: b.HTTPChkEnabled}
		for _, s := range servers {
			entry.Servers = append(entry.Servers, sv{s.Name, s.Address, s.Port, s.Weight, s.Backup})
		}
		out = append(out, entry)
	}
	return toJSON(out), nil
}

func toolListFrontends(ctx context.Context, env *Env, _ map[string]any) (string, error) {
	frontends, err := env.Store.ListFrontends(ctx)
	if err != nil {
		return "", err
	}
	backends, _ := env.Store.ListBackends(ctx)
	nameByID := map[int64]string{}
	for _, b := range backends {
		nameByID[b.ID] = b.Name
	}
	type listener struct {
		Address string
		Port    int
		SSL     bool
	}
	type fe struct {
		Name, Mode     string
		Enabled        bool
		DefaultBackend string
		Listeners      []listener
	}
	var out []fe
	for _, f := range frontends {
		entry := fe{Name: f.Name, Mode: f.Mode, Enabled: f.Enabled}
		if f.DefaultBackendID != nil {
			entry.DefaultBackend = nameByID[*f.DefaultBackendID]
		}
		binds, _ := env.Store.ListBinds(ctx, f.ID)
		for _, b := range binds {
			entry.Listeners = append(entry.Listeners, listener{b.Address, b.Port, b.SSL})
		}
		out = append(out, entry)
	}
	return toJSON(out), nil
}

func toolValidate(ctx context.Context, env *Env, _ map[string]any) (string, error) {
	config, warnings, checkOut, err := env.Preview.PreviewConfig(ctx)
	result := map[string]any{
		"valid":               err == nil,
		"line_count":          strings.Count(config, "\n"),
		"warnings":            warnings,
		"haproxy_diagnostics": strings.TrimSpace(checkOut),
	}
	if err != nil {
		result["error"] = err.Error()
	}
	return toJSON(result), nil
}

// ----------------------------------------------------------- write handlers

func toolCreateBackend(ctx context.Context, env *Env, args map[string]any) (string, error) {
	b := &store.Backend{
		Name:             argString(args, "name"),
		Mode:             argString(args, "mode"),
		Balance:          argString(args, "balance"),
		Description:      argString(args, "description"),
		Enabled:          true,
		OptionForwardFor: true,
		CookieOptions:    "insert indirect nocache",
		HTTPChkMethod:    "GET",
		HTTPChkURI:       "/",
		HTTPChkVersion:   "HTTP/1.1",
	}
	if b.Balance == "" {
		b.Balance = "roundrobin"
	}
	if argBool(args, "health_check", false) && b.Mode == "http" {
		b.HTTPChkEnabled = true
		if u := argString(args, "check_uri"); u != "" {
			b.HTTPChkURI = u
		}
		b.HTTPChkHost = argString(args, "check_host")
		if e := argString(args, "check_expect"); e != "" {
			b.CheckExpect = e
		} else {
			b.CheckExpect = "status 200"
		}
	}
	b.CookieName = argString(args, "cookie_name")

	id, err := env.Store.SaveBackend(ctx, b)
	if err != nil {
		return "", err
	}
	env.audit(ctx, "backend.created", "backend", itoa(id), b.Name)
	return fmt.Sprintf("Created backend %q (id %d). Add servers with add_server.", b.Name, id), nil
}

func toolAddServer(ctx context.Context, env *Env, args map[string]any) (string, error) {
	b, err := env.backendByName(ctx, argString(args, "backend"))
	if err != nil {
		return "", err
	}
	if b == nil {
		return "", fmt.Errorf("no backend named %q; create it first with create_backend", argString(args, "backend"))
	}
	s := &store.Server{
		BackendID:    b.ID,
		Name:         argString(args, "name"),
		Address:      argString(args, "address"),
		Port:         argInt(args, "port", 0),
		Weight:       argInt(args, "weight", 100),
		Enabled:      true,
		CheckEnabled: argBool(args, "check", true),
		CheckInter:   "2s",
		CheckRise:    2,
		CheckFall:    3,
		Backup:       argBool(args, "backup", false),
		SSL:          argBool(args, "ssl", false),
		SSLVerify:    "none",
	}
	if _, err := env.Store.SaveServer(ctx, s); err != nil {
		return "", err
	}
	env.audit(ctx, "server.created", "backend", itoa(b.ID), s.Name+" "+s.Address)
	return fmt.Sprintf("Added server %q (%s:%d) to backend %q.", s.Name, s.Address, s.Port, b.Name), nil
}

func toolCreateFrontend(ctx context.Context, env *Env, args map[string]any) (string, error) {
	f := &store.Frontend{
		Name:             argString(args, "name"),
		Mode:             argString(args, "mode"),
		Description:      argString(args, "description"),
		Enabled:          true,
		OptionForwardFor: true,
		OptionHTTPLog:    true,
		ForceHTTPS:       argBool(args, "force_https", false),
		HSTSEnabled:      argBool(args, "enable_hsts", false),
		HSTSMaxAge:       31536000,
		HSTSSubdomains:   true,
		RateLimitPeriod:  "10s",
		StatsURI:         "/haproxy-stats",
		HTTPErrorsRef:    "default",
	}
	if name := argString(args, "default_backend"); name != "" {
		b, err := env.backendByName(ctx, name)
		if err != nil {
			return "", err
		}
		if b == nil {
			return "", fmt.Errorf("no backend named %q; create it before referencing it", name)
		}
		f.DefaultBackendID = &b.ID
	}
	id, err := env.Store.SaveFrontend(ctx, f)
	if err != nil {
		return "", err
	}
	env.audit(ctx, "frontend.created", "frontend", itoa(id), f.Name)
	return fmt.Sprintf("Created frontend %q (id %d). Add listeners with add_bind.", f.Name, id), nil
}

func toolAddBind(ctx context.Context, env *Env, args map[string]any) (string, error) {
	f, err := env.frontendByName(ctx, argString(args, "frontend"))
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", fmt.Errorf("no frontend named %q; create it first with create_frontend", argString(args, "frontend"))
	}
	addr := argString(args, "address")
	if addr == "" {
		addr = "*"
	}
	b := &store.Bind{
		FrontendID: f.ID,
		Address:    addr,
		Port:       argInt(args, "port", 0),
		Enabled:    true,
		SSL:        argBool(args, "ssl", false),
		ALPN:       "h2,http/1.1",
		CertSource: "dir",
	}
	if b.SSL {
		if cert := argString(args, "certificate"); cert != "" {
			b.CertSource = "cert"
			b.CertRef = cert
		}
	}
	if _, err := env.Store.SaveBind(ctx, b); err != nil {
		return "", err
	}
	env.audit(ctx, "bind.created", "frontend", itoa(f.ID), b.Listen())
	return fmt.Sprintf("Added listener %s to frontend %q.", b.Listen(), f.Name), nil
}

func toolSetDefaultBackend(ctx context.Context, env *Env, args map[string]any) (string, error) {
	f, err := env.frontendByName(ctx, argString(args, "frontend"))
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", fmt.Errorf("no frontend named %q", argString(args, "frontend"))
	}
	b, err := env.backendByName(ctx, argString(args, "backend"))
	if err != nil {
		return "", err
	}
	if b == nil {
		return "", fmt.Errorf("no backend named %q", argString(args, "backend"))
	}
	f.DefaultBackendID = &b.ID
	if _, err := env.Store.SaveFrontend(ctx, f); err != nil {
		return "", err
	}
	env.audit(ctx, "frontend.updated", "frontend", itoa(f.ID), "default backend "+b.Name)
	return fmt.Sprintf("Frontend %q now defaults to backend %q.", f.Name, b.Name), nil
}

func toolAddRoute(ctx context.Context, env *Env, args map[string]any) (string, error) {
	f, err := env.frontendByName(ctx, argString(args, "frontend"))
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", fmt.Errorf("no frontend named %q", argString(args, "frontend"))
	}
	d := &store.Domain{
		Hostname:     argString(args, "hostname"),
		MatchType:    argString(args, "match_type"),
		PathPrefix:   argString(args, "path_prefix"),
		FrontendID:   f.ID,
		RedirectTo:   argString(args, "redirect_to"),
		RedirectCode: 301,
		ForceHTTPS:   argBool(args, "force_https", false),
		Enabled:      true,
	}
	if d.MatchType == "" {
		d.MatchType = "exact"
	}
	if name := argString(args, "backend"); name != "" {
		b, err := env.backendByName(ctx, name)
		if err != nil {
			return "", err
		}
		if b == nil {
			return "", fmt.Errorf("no backend named %q", name)
		}
		d.BackendID = &b.ID
	}
	id, err := env.Store.SaveDomain(ctx, d)
	if err != nil {
		return "", err
	}
	env.audit(ctx, "domain.created", "domain", itoa(id), d.Hostname)
	return fmt.Sprintf("Added route for %q on frontend %q.", d.Hostname, f.Name), nil
}

func toolAddACL(ctx context.Context, env *Env, args map[string]any) (string, error) {
	ownerID, err := resolveOwner(ctx, env, argString(args, "scope"), argString(args, "owner"))
	if err != nil {
		return "", err
	}
	a := &store.ACL{
		Scope:      argString(args, "scope"),
		OwnerID:    ownerID,
		Name:       argString(args, "name"),
		Expression: argString(args, "expression"),
		Enabled:    true,
	}
	if _, err := env.Store.SaveACL(ctx, a); err != nil {
		return "", err
	}
	env.audit(ctx, "acl.created", a.Scope, itoa(ownerID), a.Name)
	return fmt.Sprintf("Added ACL %q to %s %q.", a.Name, a.Scope, argString(args, "owner")), nil
}

func toolAddRule(ctx context.Context, env *Env, args map[string]any) (string, error) {
	ownerID, err := resolveOwner(ctx, env, argString(args, "scope"), argString(args, "owner"))
	if err != nil {
		return "", err
	}
	rule := &store.Rule{
		Scope:      argString(args, "scope"),
		OwnerID:    ownerID,
		Directive:  argString(args, "directive"),
		Argument:   argString(args, "argument"),
		Condition:  argString(args, "condition"),
		Enabled:    true,
		OrderIndex: 100,
	}
	if _, err := env.Store.SaveRule(ctx, rule); err != nil {
		return "", err
	}
	env.audit(ctx, "rule.created", rule.Scope, itoa(ownerID), rule.Directive+" "+rule.Argument)
	return fmt.Sprintf("Added rule '%s %s' to %s %q.", rule.Directive, rule.Argument, rule.Scope, argString(args, "owner")), nil
}

func toolCreateErrorPage(ctx context.Context, env *Env, args map[string]any) (string, error) {
	code := argInt(args, "status_code", 0)
	group := argString(args, "group")
	if group == "" {
		group = "default"
	}
	title := argString(args, "title")
	page := &store.ErrorPage{
		Name:        fmt.Sprintf("%s-%d", group, code),
		GroupName:   group,
		StatusCode:  code,
		ContentType: "text/html; charset=utf-8",
		Body:        store.DefaultErrorBody(code, title, argString(args, "message")),
		Enabled:     true,
	}
	id, err := env.Store.SaveErrorPage(ctx, page)
	if err != nil {
		return "", err
	}
	env.audit(ctx, "error_page.created", "error_page", itoa(id), page.Name)
	return fmt.Sprintf("Created error page for %d in group %q.", code, group), nil
}

func toolCreateSnippet(ctx context.Context, env *Env, args map[string]any) (string, error) {
	sn := &store.Snippet{
		Name:        argString(args, "name"),
		SectionType: argString(args, "section_type"),
		SectionArg:  argString(args, "section_arg"),
		Body:        argString(args, "body"),
		Enabled:     true,
	}
	id, err := env.Store.SaveSnippet(ctx, sn)
	if err != nil {
		return "", err
	}
	env.audit(ctx, "snippet.created", "snippet", itoa(id), sn.Name)
	return fmt.Sprintf("Created %s snippet %q. Run validate_config to make sure it parses.", sn.SectionType, sn.Name), nil
}

func toolDeleteBackend(ctx context.Context, env *Env, args map[string]any) (string, error) {
	b, err := env.backendByName(ctx, argString(args, "name"))
	if err != nil {
		return "", err
	}
	if b == nil {
		return "", fmt.Errorf("no backend named %q", argString(args, "name"))
	}
	refs, _ := env.Store.BackendReferences(ctx, b.ID)
	if len(refs) > 0 {
		return "", fmt.Errorf("backend %q is still in use: %s", b.Name, strings.Join(refs, "; "))
	}
	if err := env.Store.DeleteBackend(ctx, b.ID); err != nil {
		return "", err
	}
	env.audit(ctx, "backend.deleted", "backend", itoa(b.ID), b.Name)
	return fmt.Sprintf("Deleted backend %q.", b.Name), nil
}

func toolDeleteFrontend(ctx context.Context, env *Env, args map[string]any) (string, error) {
	f, err := env.frontendByName(ctx, argString(args, "name"))
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", fmt.Errorf("no frontend named %q", argString(args, "name"))
	}
	if err := env.Store.DeleteFrontend(ctx, f.ID); err != nil {
		return "", err
	}
	env.audit(ctx, "frontend.deleted", "frontend", itoa(f.ID), f.Name)
	return fmt.Sprintf("Deleted frontend %q.", f.Name), nil
}

// resolveOwner turns a (scope, name) pair into an owner id.
func resolveOwner(ctx context.Context, env *Env, scope, name string) (int64, error) {
	switch scope {
	case "frontend":
		f, err := env.frontendByName(ctx, name)
		if err != nil {
			return 0, err
		}
		if f == nil {
			return 0, fmt.Errorf("no frontend named %q", name)
		}
		return f.ID, nil
	case "backend":
		b, err := env.backendByName(ctx, name)
		if err != nil {
			return 0, err
		}
		if b == nil {
			return 0, fmt.Errorf("no backend named %q", name)
		}
		return b.ID, nil
	}
	return 0, fmt.Errorf("scope must be 'frontend' or 'backend', got %q", scope)
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("{\"error\":%q}", err.Error())
	}
	return string(b)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
