package web

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

// ------------------------------------------------------------- certificates

func (s *Server) handleCerts(w http.ResponseWriter, r *http.Request) {
	certs, err := s.store.ListCertificates(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	p := s.newPage(r, "Certificates", "certificates")
	p.Flash = s.takeFlash(w, r)
	p.Data["Certificates"] = certs
	p.Data["CertsDir"] = s.cfg.CertsDir
	s.render(w, r, "certificates.html", p)
}

func (s *Server) handleCertForm(w http.ResponseWriter, r *http.Request) {
	c := &store.Certificate{}
	if raw := r.PathValue("id"); raw != "" {
		id, err := pathID(r, "id")
		if err != nil {
			s.notFound(w, r, "That certificate")
			return
		}
		c, err = s.store.GetCertificate(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r, "That certificate")
				return
			}
			s.renderError(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}

	title := "Add certificate"
	if c.ID != 0 {
		title = "Certificate: " + c.Name
	}
	p := s.newPage(r, title, "certificates")
	p.Flash = s.takeFlash(w, r)
	p.Data["Certificate"] = c
	s.render(w, r, "certificate_edit.html", p)
}

// maxCertUpload bounds a PEM upload; real certificates are a few KiB.
const maxCertUpload = 256 << 10

func (s *Server) handleCertSave(w http.ResponseWriter, r *http.Request) {
	// The multipart body was already parsed by the CSRF check, so both the
	// pasted textareas and the uploaded files are available here.
	c := &store.Certificate{
		ID:       int64(formInt(r, "id", 0)),
		Name:     formStr(r, "name"),
		CertPEM:  pemField(r, "cert_pem", "cert_file"),
		KeyPEM:   pemField(r, "key_pem", "key_file"),
		ChainPEM: pemField(r, "chain_pem", "chain_file"),
	}

	back := "/certificates/new"
	if c.ID != 0 {
		back = "/certificates/" + int64str(c.ID)
	}

	// Keep the stored key when editing metadata without re-pasting it.
	if c.ID != 0 && c.KeyPEM == "" {
		if existing, err := s.store.GetCertificate(r.Context(), c.ID); err == nil {
			if c.CertPEM == "" {
				c.CertPEM = existing.CertPEM
			}
			c.KeyPEM = existing.KeyPEM
			if c.ChainPEM == "" {
				c.ChainPEM = existing.ChainPEM
			}
		}
	}

	id, err := s.store.SaveCertificate(r.Context(), c)
	if err != nil {
		s.fail(w, r, back, err)
		return
	}

	action := "certificate.created"
	if c.ID != 0 {
		action = "certificate.updated"
	}
	s.audit(r, action, "certificate", int64str(id), c.Name)
	s.setFlash(w, r, "success", "Certificate "+c.Name+" saved. Apply the configuration to deploy it.", "")
	redirect(w, r, "/certificates")
}

// pemField prefers an uploaded file over the pasted textarea.
func pemField(r *http.Request, textField, fileField string) string {
	if r.MultipartForm != nil {
		if files := r.MultipartForm.File[fileField]; len(files) > 0 {
			f, err := files[0].Open()
			if err == nil {
				defer f.Close()
				data, err := io.ReadAll(io.LimitReader(f, maxCertUpload))
				if err == nil && strings.TrimSpace(string(data)) != "" {
					return strings.TrimSpace(string(data))
				}
			}
		}
	}
	return strings.TrimSpace(r.PostFormValue(textField))
}

func (s *Server) handleCertDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That certificate")
		return
	}
	c, err := s.store.GetCertificate(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That certificate")
		return
	}
	if err := s.store.DeleteCertificate(r.Context(), id); err != nil {
		s.fail(w, r, "/certificates", err)
		return
	}
	s.audit(r, "certificate.deleted", "certificate", int64str(id), c.Name)
	s.setFlash(w, r, "success", "Certificate "+c.Name+" deleted.",
		"The file is removed from disk on the next apply.")
	redirect(w, r, "/certificates")
}

// -------------------------------------------------------------- error pages

func (s *Server) handleErrorPages(w http.ResponseWriter, r *http.Request) {
	pages, err := s.store.ListErrorPages(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Group for display so each http-errors section reads as a unit.
	type group struct {
		Name  string
		Pages []*store.ErrorPage
	}
	var groups []group
	seen := map[string]int{}
	for _, pg := range pages {
		idx, ok := seen[pg.GroupName]
		if !ok {
			groups = append(groups, group{Name: pg.GroupName})
			idx = len(groups) - 1
			seen[pg.GroupName] = idx
		}
		groups[idx].Pages = append(groups[idx].Pages, pg)
	}

	p := s.newPage(r, "Error pages", "error-pages")
	p.Flash = s.takeFlash(w, r)
	p.Data["Groups"] = groups
	p.Data["ErrorPagesDir"] = s.cfg.ErrorPagesDir
	s.render(w, r, "error_pages.html", p)
}

func (s *Server) handleErrorPageForm(w http.ResponseWriter, r *http.Request) {
	pg := &store.ErrorPage{
		Enabled: true, GroupName: "default", StatusCode: 503,
		ContentType: "text/html; charset=utf-8",
		Body:        store.DefaultErrorBody(503, "Service Unavailable", "No server is available to handle this request right now."),
	}

	if raw := r.PathValue("id"); raw != "" {
		id, err := pathID(r, "id")
		if err != nil {
			s.notFound(w, r, "That error page")
			return
		}
		pg, err = s.store.GetErrorPage(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r, "That error page")
				return
			}
			s.renderError(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}

	groups, _ := s.store.ErrorPageGroups(r.Context())
	title := "New error page"
	if pg.ID != 0 {
		title = "Error page: " + pg.Name
	}

	p := s.newPage(r, title, "error-pages")
	p.Flash = s.takeFlash(w, r)
	p.Data["Page"] = pg
	p.Data["Codes"] = store.ErrorCodes
	p.Data["Groups"] = groups
	s.render(w, r, "error_page_edit.html", p)
}

func (s *Server) handleErrorPageSave(w http.ResponseWriter, r *http.Request) {
	pg := &store.ErrorPage{
		ID:          int64(formInt(r, "id", 0)),
		Name:        formStr(r, "name"),
		GroupName:   formStr(r, "group_name"),
		StatusCode:  formInt(r, "status_code", 503),
		ContentType: formStr(r, "content_type"),
		Headers:     normalizeLines(formStr(r, "headers")),
		Body:        normalizeBlock(r.PostFormValue("body")),
		Enabled:     formBool(r, "enabled"),
	}
	if pg.GroupName == "" {
		pg.GroupName = "default"
	}

	back := "/error-pages/new"
	if pg.ID != 0 {
		back = "/error-pages/" + int64str(pg.ID)
	}

	id, err := s.store.SaveErrorPage(r.Context(), pg)
	if err != nil {
		s.fail(w, r, back, err)
		return
	}

	action := "error_page.created"
	if pg.ID != 0 {
		action = "error_page.updated"
	}
	s.audit(r, action, "error_page", int64str(id), pg.Name)
	s.setFlash(w, r, "success", "Error page "+pg.Name+" saved. Apply the configuration to deploy it.", "")
	redirect(w, r, "/error-pages/"+int64str(id))
}

// handleErrorPagePreview serves the stored body so the operator can see the
// page exactly as a visitor would.
func (s *Server) handleErrorPagePreview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That error page")
		return
	}
	pg, err := s.store.GetErrorPage(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That error page")
		return
	}

	// The body is operator-authored HTML. It is served from this origin, so
	// it is sandboxed with a CSP that blocks scripts and outbound requests.
	w.Header().Set("Content-Type", pg.ContentType)
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, pg.Body)
}

func (s *Server) handleErrorPageDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That error page")
		return
	}
	pg, err := s.store.GetErrorPage(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That error page")
		return
	}
	if err := s.store.DeleteErrorPage(r.Context(), id); err != nil {
		s.fail(w, r, "/error-pages", err)
		return
	}
	s.audit(r, "error_page.deleted", "error_page", int64str(id), pg.Name)
	s.setFlash(w, r, "success", "Error page "+pg.Name+" deleted.", "")
	redirect(w, r, "/error-pages")
}

// ----------------------------------------------------------------- snippets

func (s *Server) handleSnippets(w http.ResponseWriter, r *http.Request) {
	snippets, err := s.store.ListSnippets(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	p := s.newPage(r, "Snippets", "snippets")
	p.Flash = s.takeFlash(w, r)
	p.Data["Snippets"] = snippets
	s.render(w, r, "snippets.html", p)
}

func (s *Server) handleSnippetForm(w http.ResponseWriter, r *http.Request) {
	sn := &store.Snippet{Enabled: true, SectionType: "raw"}
	if raw := r.PathValue("id"); raw != "" {
		id, err := pathID(r, "id")
		if err != nil {
			s.notFound(w, r, "That snippet")
			return
		}
		sn, err = s.store.GetSnippet(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r, "That snippet")
				return
			}
			s.renderError(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}

	title := "New snippet"
	if sn.ID != 0 {
		title = "Snippet: " + sn.Name
	}
	p := s.newPage(r, title, "snippets")
	p.Flash = s.takeFlash(w, r)
	p.Data["Snippet"] = sn
	p.Data["SectionTypes"] = store.SectionTypes
	s.render(w, r, "snippet_edit.html", p)
}

func (s *Server) handleSnippetSave(w http.ResponseWriter, r *http.Request) {
	sn := &store.Snippet{
		ID:          int64(formInt(r, "id", 0)),
		Name:        formStr(r, "name"),
		SectionType: formStr(r, "section_type"),
		SectionArg:  formStr(r, "section_arg"),
		Body:        normalizeBlock(r.PostFormValue("body")),
		Enabled:     formBool(r, "enabled"),
		OrderIndex:  formInt(r, "order_index", 0),
	}

	back := "/snippets/new"
	if sn.ID != 0 {
		back = "/snippets/" + int64str(sn.ID)
	}

	id, err := s.store.SaveSnippet(r.Context(), sn)
	if err != nil {
		s.fail(w, r, back, err)
		return
	}
	action := "snippet.created"
	if sn.ID != 0 {
		action = "snippet.updated"
	}
	s.audit(r, action, "snippet", int64str(id), sn.SectionType+" "+sn.Name)
	s.setFlash(w, r, "success", "Snippet "+sn.Name+" saved. Validate before applying.", "")
	redirect(w, r, "/snippets/"+int64str(id))
}

func (s *Server) handleSnippetDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That snippet")
		return
	}
	sn, err := s.store.GetSnippet(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That snippet")
		return
	}
	if err := s.store.DeleteSnippet(r.Context(), id); err != nil {
		s.fail(w, r, "/snippets", err)
		return
	}
	s.audit(r, "snippet.deleted", "snippet", int64str(id), sn.Name)
	s.setFlash(w, r, "success", "Snippet "+sn.Name+" deleted.", "")
	redirect(w, r, "/snippets")
}

// --------------------------------------------------------- global/defaults

func (s *Server) handleGlobal(w http.ResponseWriter, r *http.Request) {
	g, err := s.store.GetGlobal(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	p := s.newPage(r, "Global settings", "global")
	p.Flash = s.takeFlash(w, r)
	p.Data["Global"] = g
	s.render(w, r, "global.html", p)
}

func (s *Server) handleGlobalSave(w http.ResponseWriter, r *http.Request) {
	g := &store.GlobalConfig{
		Maxconn:                    formInt(r, "maxconn", 20000),
		Nbthread:                   formInt(r, "nbthread", 0),
		RunUser:                    formStr(r, "run_user"),
		RunGroup:                   formStr(r, "run_group"),
		Chroot:                     formStr(r, "chroot"),
		Daemon:                     formBool(r, "daemon"),
		LogTargets:                 normalizeLines(formStr(r, "log_targets")),
		StatsSocket:                formStr(r, "stats_socket"),
		StatsTimeout:               formStr(r, "stats_timeout"),
		HardStopAfter:              formStr(r, "hard_stop_after"),
		SSLDefaultBindCiphers:      formStr(r, "ssl_default_bind_ciphers"),
		SSLDefaultBindCiphersuites: formStr(r, "ssl_default_bind_ciphersuites"),
		SSLDefaultBindOptions:      formStr(r, "ssl_default_bind_options"),
		SSLDefaultServerCiphers:    formStr(r, "ssl_default_server_ciphers"),
		SSLDefaultServerOptions:    formStr(r, "ssl_default_server_options"),
		TuneSSLDefaultDHParam:      formInt(r, "tune_ssl_default_dh_param", 2048),
		SSLDHParamFile:             formStr(r, "ssl_dh_param_file"),
		Extra:                      normalizeBlock(r.PostFormValue("extra")),
	}

	if err := s.store.SaveGlobal(r.Context(), g); err != nil {
		s.fail(w, r, "/global", err)
		return
	}
	s.audit(r, "global.updated", "global", "1", "")
	s.setFlash(w, r, "success", "Global settings saved. Apply the configuration to make them live.", "")
	redirect(w, r, "/global")
}

func (s *Server) handleDefaults(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListDefaults(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	groups, _ := s.store.ErrorPageGroups(r.Context())

	p := s.newPage(r, "Defaults", "defaults")
	p.Flash = s.takeFlash(w, r)
	p.Data["Defaults"] = list
	p.Data["ErrorGroups"] = groups
	s.render(w, r, "defaults.html", p)
}

func (s *Server) handleDefaultsSave(w http.ResponseWriter, r *http.Request) {
	d := &store.DefaultsConfig{
		ID:                   int64(formInt(r, "id", 0)),
		Name:                 formStr(r, "name"),
		Enabled:              formBool(r, "enabled"),
		Mode:                 formStr(r, "mode"),
		LogGlobal:            formBool(r, "log_global"),
		Options:              normalizeLines(formStr(r, "options")),
		Retries:              formInt(r, "retries", 3),
		Maxconn:              formInt(r, "maxconn", 0),
		TimeoutConnect:       formStr(r, "timeout_connect"),
		TimeoutClient:        formStr(r, "timeout_client"),
		TimeoutServer:        formStr(r, "timeout_server"),
		TimeoutHTTPRequest:   formStr(r, "timeout_http_request"),
		TimeoutHTTPKeepAlive: formStr(r, "timeout_http_keep_alive"),
		TimeoutQueue:         formStr(r, "timeout_queue"),
		TimeoutCheck:         formStr(r, "timeout_check"),
		TimeoutTunnel:        formStr(r, "timeout_tunnel"),
		Compression:          formStr(r, "compression"),
		ErrorFilesRef:        formStr(r, "error_files_ref"),
		Extra:                normalizeBlock(r.PostFormValue("extra")),
		OrderIndex:           formInt(r, "order_index", 0),
	}

	if _, err := s.store.SaveDefaults(r.Context(), d); err != nil {
		s.fail(w, r, "/defaults", err)
		return
	}
	s.audit(r, "defaults.saved", "defaults", int64str(d.ID), d.Name)
	s.setFlash(w, r, "success", "Defaults saved. Apply the configuration to make them live.", "")
	redirect(w, r, "/defaults")
}

func (s *Server) handleDefaultsDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That defaults section")
		return
	}
	if err := s.store.DeleteDefaults(r.Context(), id); err != nil {
		s.fail(w, r, "/defaults", err)
		return
	}
	s.audit(r, "defaults.deleted", "defaults", int64str(id), "")
	s.setFlash(w, r, "success", "Defaults section deleted.", "")
	redirect(w, r, "/defaults")
}
