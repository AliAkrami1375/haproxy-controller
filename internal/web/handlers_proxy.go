package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

// audit records an action performed by the signed-in user.
func (s *Server) audit(r *http.Request, action, entity, entityID, detail string) {
	u := userFrom(r)
	e := store.AuditEntry{
		Username: "system", Action: action, Entity: entity,
		EntityID: entityID, Detail: detail, IP: s.requestIP(r),
	}
	if u != nil {
		e.UserID = &u.ID
		e.Username = u.Username
	}
	_ = s.store.Audit(r.Context(), e)
}

// fail flashes an error and redirects, the standard failure path for a POST.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, path string, err error) {
	s.setFlash(w, r, "error", err.Error(), "")
	redirect(w, r, path)
}

// notFound renders the 404 page for a missing record.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request, what string) {
	s.renderError(w, r, http.StatusNotFound, what+" was not found.")
}

// ---------------------------------------------------------------- frontends

func (s *Server) handleFrontends(w http.ResponseWriter, r *http.Request) {
	frontends, err := s.store.ListFrontends(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	backends, err := s.store.ListBackends(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	byID := map[int64]string{}
	for _, b := range backends {
		byID[b.ID] = b.Name
	}

	// Attach binds so the listing can show what each frontend listens on.
	type row struct {
		*store.Frontend
		Binds       []store.Bind
		BackendName string
		RouteCount  int
	}
	var rows []row
	for _, f := range frontends {
		binds, _ := s.store.ListBinds(r.Context(), f.ID)
		domains, _ := s.store.ListDomainsByFrontend(r.Context(), f.ID)
		name := ""
		if f.DefaultBackendID != nil {
			name = byID[*f.DefaultBackendID]
		}
		rows = append(rows, row{Frontend: f, Binds: binds, BackendName: name, RouteCount: len(domains)})
	}

	p := s.newPage(r, "Frontends", "frontends")
	p.Flash = s.takeFlash(w, r)
	p.Data["Rows"] = rows
	s.render(w, r, "frontends.html", p)
}

func (s *Server) handleFrontendForm(w http.ResponseWriter, r *http.Request) {
	f := &store.Frontend{
		Enabled: true, Mode: "http", OptionForwardFor: true, OptionHTTPLog: true,
		HSTSMaxAge: 31536000, RateLimitPeriod: "10s", StatsURI: "/haproxy-stats",
	}

	if raw := r.PathValue("id"); raw != "" {
		id, err := pathID(r, "id")
		if err != nil {
			s.notFound(w, r, "That frontend")
			return
		}
		f, err = s.store.GetFrontendFull(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r, "That frontend")
				return
			}
			s.renderError(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}

	backends, _ := s.store.ListBackends(r.Context())
	certs, _ := s.store.ListCertificates(r.Context())
	groups, _ := s.store.ErrorPageGroups(r.Context())

	title := "New frontend"
	if f.ID != 0 {
		title = "Frontend: " + f.Name
	}
	p := s.newPage(r, title, "frontends")
	p.Flash = s.takeFlash(w, r)
	p.Data["Frontend"] = f
	p.Data["Backends"] = backends
	p.Data["Certificates"] = certs
	p.Data["ErrorGroups"] = groups
	p.Data["Directives"] = store.AllowedDirectives
	p.Data["ControlPort"] = s.cfg.ListenPort
	s.render(w, r, "frontend_edit.html", p)
}

func (s *Server) handleFrontendSave(w http.ResponseWriter, r *http.Request) {
	f := &store.Frontend{
		ID:               int64(formInt(r, "id", 0)),
		Name:             formStr(r, "name"),
		Description:      formStr(r, "description"),
		Enabled:          formBool(r, "enabled"),
		Mode:             formStr(r, "mode"),
		DefaultBackendID: formInt64Ptr(r, "default_backend_id"),
		Maxconn:          formInt(r, "maxconn", 0),
		OptionForwardFor: formBool(r, "option_forwardfor"),
		OptionHTTPLog:    formBool(r, "option_httplog"),
		OptionHTTPClose:  formBool(r, "option_http_close"),
		ForceHTTPS:       formBool(r, "force_https"),
		HSTSEnabled:      formBool(r, "hsts_enabled"),
		HSTSMaxAge:       formInt(r, "hsts_max_age", 31536000),
		HSTSSubdomains:   formBool(r, "hsts_subdomains"),
		HSTSPreload:      formBool(r, "hsts_preload"),
		RateLimitEnabled: formBool(r, "rate_limit_enabled"),
		RateLimitRPS:     formInt(r, "rate_limit_rps", 0),
		RateLimitPeriod:  formStr(r, "rate_limit_period"),
		StatsEnabled:     formBool(r, "stats_enabled"),
		StatsURI:         formStr(r, "stats_uri"),
		StatsAuth:        formStr(r, "stats_auth"),
		HTTPErrorsRef:    formStr(r, "http_errors_ref"),
		LogSettings:      normalizeLines(formStr(r, "log_settings")),
		Extra:            normalizeBlock(r.PostFormValue("extra")),
		OrderIndex:       formInt(r, "order_index", 0),
	}

	back := "/frontends/new"
	if f.ID != 0 {
		back = "/frontends/" + int64str(f.ID)
	}

	id, err := s.store.SaveFrontend(r.Context(), f)
	if err != nil {
		s.fail(w, r, back, err)
		return
	}

	action := "frontend.created"
	if f.ID != 0 {
		action = "frontend.updated"
	}
	s.audit(r, action, "frontend", int64str(id), f.Name)
	s.setFlash(w, r, "success", "Frontend "+f.Name+" saved. Apply the configuration to make it live.", "")
	redirect(w, r, "/frontends/"+int64str(id))
}

func (s *Server) handleFrontendDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That frontend")
		return
	}
	f, err := s.store.GetFrontend(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That frontend")
		return
	}
	if err := s.store.DeleteFrontend(r.Context(), id); err != nil {
		s.fail(w, r, "/frontends", err)
		return
	}
	s.audit(r, "frontend.deleted", "frontend", int64str(id), f.Name)
	s.setFlash(w, r, "success", "Frontend "+f.Name+" deleted.", "")
	redirect(w, r, "/frontends")
}

// -------------------------------------------------------------------- binds

func (s *Server) handleBindSave(w http.ResponseWriter, r *http.Request) {
	frontendID, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That frontend")
		return
	}
	back := "/frontends/" + int64str(frontendID)

	b := &store.Bind{
		ID:          int64(formInt(r, "bind_id", 0)),
		FrontendID:  frontendID,
		Address:     formStr(r, "address"),
		Port:        formInt(r, "port", 0),
		Enabled:     formBool(r, "enabled"),
		SSL:         formBool(r, "ssl"),
		CertSource:  formStr(r, "cert_source"),
		CertRef:     formStr(r, "cert_ref"),
		ALPN:        formStr(r, "alpn"),
		AcceptProxy: formBool(r, "accept_proxy"),
		Transparent: formBool(r, "transparent"),
		ExtraParams: formStr(r, "extra_params"),
		OrderIndex:  formInt(r, "order_index", 0),
	}
	if b.Address == "" {
		b.Address = "*"
	}

	// Refuse to hand HAProxy the port the control panel is listening on:
	// doing so would lock the operator out of the panel after apply.
	if b.Enabled && b.Port == s.cfg.ListenPort && addressOverlaps(b.Address, s.cfg.ListenAddr) {
		s.fail(w, r, back, errors.New(
			"port "+itoa(b.Port)+" is in use by this control panel; pick another port or move the panel first"))
		return
	}

	if _, err := s.store.SaveBind(r.Context(), b); err != nil {
		s.fail(w, r, back, err)
		return
	}
	s.audit(r, "bind.saved", "frontend", int64str(frontendID), b.Listen())
	s.setFlash(w, r, "success", "Listener "+b.Listen()+" saved.", "")
	redirect(w, r, back)
}

// addressOverlaps reports whether two bind addresses can collide, treating
// wildcard forms as overlapping everything.
func addressOverlaps(a, b string) bool {
	norm := func(v string) string {
		v = strings.TrimSpace(v)
		if v == "" || v == "*" || v == "0.0.0.0" || v == "::" {
			return "*"
		}
		return v
	}
	na, nb := norm(a), norm(b)
	return na == "*" || nb == "*" || na == nb
}

func (s *Server) handleBindDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That listener")
		return
	}
	b, err := s.store.GetBind(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That listener")
		return
	}
	if err := s.store.DeleteBind(r.Context(), id); err != nil {
		s.fail(w, r, "/frontends/"+int64str(b.FrontendID), err)
		return
	}
	s.audit(r, "bind.deleted", "frontend", int64str(b.FrontendID), b.Listen())
	s.setFlash(w, r, "success", "Listener "+b.Listen()+" removed.", "")
	redirect(w, r, "/frontends/"+int64str(b.FrontendID))
}

// ----------------------------------------------------------------- backends

func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	backends, err := s.store.ListBackends(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	type row struct {
		*store.Backend
		Servers []store.Server
	}
	var rows []row
	for _, b := range backends {
		servers, _ := s.store.ListServers(r.Context(), b.ID)
		rows = append(rows, row{Backend: b, Servers: servers})
	}

	p := s.newPage(r, "Backends", "backends")
	p.Flash = s.takeFlash(w, r)
	p.Data["Rows"] = rows
	s.render(w, r, "backends.html", p)
}

func (s *Server) handleBackendForm(w http.ResponseWriter, r *http.Request) {
	b := &store.Backend{
		Enabled: true, Mode: "http", Balance: "roundrobin",
		OptionForwardFor: true, HTTPChkMethod: "GET", HTTPChkURI: "/",
		HTTPChkVersion: "HTTP/1.1", CookieOptions: "insert indirect nocache",
	}

	if raw := r.PathValue("id"); raw != "" {
		id, err := pathID(r, "id")
		if err != nil {
			s.notFound(w, r, "That backend")
			return
		}
		b, err = s.store.GetBackendFull(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r, "That backend")
				return
			}
			s.renderError(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}

	groups, _ := s.store.ErrorPageGroups(r.Context())
	title := "New backend"
	if b.ID != 0 {
		title = "Backend: " + b.Name
	}

	p := s.newPage(r, title, "backends")
	p.Flash = s.takeFlash(w, r)
	p.Data["Backend"] = b
	p.Data["ErrorGroups"] = groups
	p.Data["Directives"] = store.AllowedDirectives
	p.Data["BalanceAlgorithms"] = []string{
		"roundrobin", "static-rr", "leastconn", "first", "source",
		"uri", "url_param", "hdr", "random", "rdp-cookie",
	}
	s.render(w, r, "backend_edit.html", p)
}

func (s *Server) handleBackendSave(w http.ResponseWriter, r *http.Request) {
	b := &store.Backend{
		ID:               int64(formInt(r, "id", 0)),
		Name:             formStr(r, "name"),
		Description:      formStr(r, "description"),
		Enabled:          formBool(r, "enabled"),
		Mode:             formStr(r, "mode"),
		Balance:          formStr(r, "balance"),
		BalanceParam:     formStr(r, "balance_param"),
		OptionForwardFor: formBool(r, "option_forwardfor"),
		OptionHTTPClose:  formBool(r, "option_http_close"),
		HTTPChkEnabled:   formBool(r, "httpchk_enabled"),
		HTTPChkMethod:    formStr(r, "httpchk_method"),
		HTTPChkURI:       formStr(r, "httpchk_uri"),
		HTTPChkVersion:   formStr(r, "httpchk_version"),
		HTTPChkHost:      formStr(r, "httpchk_host"),
		CheckExpect:      formStr(r, "check_expect"),
		TCPChkEnabled:    formBool(r, "tcpchk_enabled"),
		CookieName:       formStr(r, "cookie_name"),
		CookieOptions:    formStr(r, "cookie_options"),
		StickEnabled:     formBool(r, "stick_enabled"),
		StickTable:       formStr(r, "stick_table"),
		StickOn:          formStr(r, "stick_on"),
		Retries:          formInt(r, "retries", 0),
		TimeoutConnect:   formStr(r, "timeout_connect"),
		TimeoutServer:    formStr(r, "timeout_server"),
		TimeoutCheck:     formStr(r, "timeout_check"),
		HTTPErrorsRef:    formStr(r, "http_errors_ref"),
		Extra:            normalizeBlock(r.PostFormValue("extra")),
		OrderIndex:       formInt(r, "order_index", 0),
	}

	back := "/backends/new"
	if b.ID != 0 {
		back = "/backends/" + int64str(b.ID)
	}

	id, err := s.store.SaveBackend(r.Context(), b)
	if err != nil {
		s.fail(w, r, back, err)
		return
	}

	action := "backend.created"
	if b.ID != 0 {
		action = "backend.updated"
	}
	s.audit(r, action, "backend", int64str(id), b.Name)
	s.setFlash(w, r, "success", "Backend "+b.Name+" saved. Apply the configuration to make it live.", "")
	redirect(w, r, "/backends/"+int64str(id))
}

func (s *Server) handleBackendDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That backend")
		return
	}
	b, err := s.store.GetBackend(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That backend")
		return
	}

	// Refuse while anything still routes to it, rather than silently
	// breaking that route on the next apply.
	refs, err := s.store.BackendReferences(r.Context(), id)
	if err == nil && len(refs) > 0 {
		s.fail(w, r, "/backends/"+int64str(id), errors.New(
			"backend "+b.Name+" is still in use: "+strings.Join(refs, "; ")))
		return
	}

	if err := s.store.DeleteBackend(r.Context(), id); err != nil {
		s.fail(w, r, "/backends", err)
		return
	}
	s.audit(r, "backend.deleted", "backend", int64str(id), b.Name)
	s.setFlash(w, r, "success", "Backend "+b.Name+" deleted.", "")
	redirect(w, r, "/backends")
}

// ------------------------------------------------------------------ servers

func (s *Server) handleServerSave(w http.ResponseWriter, r *http.Request) {
	backendID, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That backend")
		return
	}
	back := "/backends/" + int64str(backendID)

	sv := &store.Server{
		ID:           int64(formInt(r, "server_id", 0)),
		BackendID:    backendID,
		Name:         formStr(r, "name"),
		Address:      formStr(r, "address"),
		Port:         formInt(r, "port", 0),
		Enabled:      formBool(r, "enabled"),
		Weight:       formInt(r, "weight", 100),
		Maxconn:      formInt(r, "maxconn", 0),
		CheckEnabled: formBool(r, "check_enabled"),
		CheckInter:   formStr(r, "check_inter"),
		CheckRise:    formInt(r, "check_rise", 2),
		CheckFall:    formInt(r, "check_fall", 3),
		SSL:          formBool(r, "ssl"),
		SSLVerify:    formStr(r, "ssl_verify"),
		SNI:          formStr(r, "sni"),
		Backup:       formBool(r, "backup"),
		SendProxy:    formStr(r, "send_proxy"),
		CookieValue:  formStr(r, "cookie_value"),
		ExtraParams:  formStr(r, "extra_params"),
		OrderIndex:   formInt(r, "order_index", 0),
	}

	if _, err := s.store.SaveServer(r.Context(), sv); err != nil {
		s.fail(w, r, back, err)
		return
	}
	s.audit(r, "server.saved", "backend", int64str(backendID), sv.Name+" "+sv.Address)
	s.setFlash(w, r, "success", "Server "+sv.Name+" saved.", "")
	redirect(w, r, back)
}

func (s *Server) handleServerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That server")
		return
	}
	sv, err := s.store.GetServer(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That server")
		return
	}
	if err := s.store.DeleteServer(r.Context(), id); err != nil {
		s.fail(w, r, "/backends/"+int64str(sv.BackendID), err)
		return
	}
	s.audit(r, "server.deleted", "backend", int64str(sv.BackendID), sv.Name)
	s.setFlash(w, r, "success", "Server "+sv.Name+" removed.", "")
	redirect(w, r, "/backends/"+int64str(sv.BackendID))
}

// handleServerState drives the Runtime API so an operator can drain or
// re-enable a server immediately, without reloading HAProxy.
func (s *Server) handleServerState(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That server")
		return
	}
	sv, err := s.store.GetServer(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That server")
		return
	}
	b, err := s.store.GetBackend(r.Context(), sv.BackendID)
	if err != nil {
		s.notFound(w, r, "That backend")
		return
	}

	back := "/backends/" + int64str(sv.BackendID)
	state := formStr(r, "state")
	if err := s.runtime.SetServerState(r.Context(), b.Name, sv.Name, state); err != nil {
		s.fail(w, r, back, err)
		return
	}
	s.audit(r, "server.state", "backend", int64str(b.ID), sv.Name+" -> "+state)
	s.setFlash(w, r, "success", "Server "+sv.Name+" set to "+state+".",
		"This change is live now but is not stored; it resets on the next reload.")
	redirect(w, r, back)
}
