// Package web serves the control panel.
package web

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ebdaa/haproxy-controller/internal/config"
	"github.com/ebdaa/haproxy-controller/internal/hap"
	"github.com/ebdaa/haproxy-controller/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server owns the control panel listener and its dependencies.
type Server struct {
	cfg       *config.Config
	store     *store.Store
	deployer  *hap.Deployer
	runtime   *hap.Runtime
	validator *hap.Validator
	log       *slog.Logger
	tpl       *template.Template

	mu       sync.Mutex
	http     *http.Server
	listener net.Listener
	rebind   chan struct{}
}

// Options bundles the dependencies a Server needs.
type Options struct {
	Config    *config.Config
	Store     *store.Store
	Deployer  *hap.Deployer
	Runtime   *hap.Runtime
	Validator *hap.Validator
	Logger    *slog.Logger
}

// NewServer wires up the control panel.
func NewServer(o Options) (*Server, error) {
	tpl, err := parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{
		cfg:       o.Config,
		store:     o.Store,
		deployer:  o.Deployer,
		runtime:   o.Runtime,
		validator: o.Validator,
		log:       o.Logger,
		tpl:       tpl,
		rebind:    make(chan struct{}, 1),
	}, nil
}

// Run serves until the context is cancelled. When the operator changes the
// listen address or port, the listener is swapped without dropping the
// process, so an in-flight settings save can report success.
func (s *Server) Run(ctx context.Context) error {
	for {
		addr := s.cfg.Listener()
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", addr, err)
		}

		srv := &http.Server{
			Handler:           s.routes(),
			ReadHeaderTimeout: 15 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      120 * time.Second,
			IdleTimeout:       120 * time.Second,
			ErrorLog:          nil,
		}

		useTLS, certFile, keyFile := s.cfg.TLS()
		if useTLS {
			srv.TLSConfig = &tls.Config{
				MinVersion: tls.VersionTLS12,
				CipherSuites: []uint16{
					tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
					tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
				},
			}
		}

		s.mu.Lock()
		s.http = srv
		s.listener = ln
		s.mu.Unlock()

		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		s.log.Info("control panel listening", "url", fmt.Sprintf("%s://%s/", scheme, addr))

		serveErr := make(chan error, 1)
		go func() {
			if useTLS {
				serveErr <- srv.ServeTLS(ln, certFile, keyFile)
			} else {
				serveErr <- srv.Serve(ln)
			}
		}()

		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := srv.Shutdown(shutdownCtx)
			cancel()
			return err

		case <-s.rebind:
			s.log.Info("rebinding control panel listener")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = srv.Shutdown(shutdownCtx)
			cancel()
			<-serveErr
			// Loop around and listen on the new address.

		case err := <-serveErr:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		}
	}
}

// Rebind asks Run to restart the listener, picking up a changed address, port
// or TLS setting. It never blocks.
func (s *Server) Rebind() {
	select {
	case s.rebind <- struct{}{}:
	default:
	}
}

// routes builds the HTTP handler tree.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Static assets are embedded, so they are served straight from the binary.
	mux.Handle("GET /static/", http.StripPrefix("/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Authentication.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.requireAuth(s.handleLogout))

	// Dashboard and status.
	mux.HandleFunc("GET /{$}", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("GET /status", s.requireAuth(s.handleStatus))
	mux.HandleFunc("GET /api/status", s.requireAuth(s.handleStatusJSON))

	// Frontends.
	mux.HandleFunc("GET /frontends", s.requireAuth(s.handleFrontends))
	mux.HandleFunc("GET /frontends/new", s.requireEdit(s.handleFrontendForm))
	mux.HandleFunc("GET /frontends/{id}", s.requireAuth(s.handleFrontendForm))
	mux.HandleFunc("POST /frontends", s.requireEdit(s.handleFrontendSave))
	mux.HandleFunc("POST /frontends/{id}/delete", s.requireEdit(s.handleFrontendDelete))
	mux.HandleFunc("POST /frontends/{id}/binds", s.requireEdit(s.handleBindSave))
	mux.HandleFunc("POST /binds/{id}/delete", s.requireEdit(s.handleBindDelete))

	// Backends.
	mux.HandleFunc("GET /backends", s.requireAuth(s.handleBackends))
	mux.HandleFunc("GET /backends/new", s.requireEdit(s.handleBackendForm))
	mux.HandleFunc("GET /backends/{id}", s.requireAuth(s.handleBackendForm))
	mux.HandleFunc("POST /backends", s.requireEdit(s.handleBackendSave))
	mux.HandleFunc("POST /backends/{id}/delete", s.requireEdit(s.handleBackendDelete))
	mux.HandleFunc("POST /backends/{id}/servers", s.requireEdit(s.handleServerSave))
	mux.HandleFunc("POST /servers/{id}/delete", s.requireEdit(s.handleServerDelete))
	mux.HandleFunc("POST /servers/{id}/state", s.requireEdit(s.handleServerState))

	// Routing: ACLs and ordered rules attach to either proxy kind.
	mux.HandleFunc("POST /acls", s.requireEdit(s.handleACLSave))
	mux.HandleFunc("POST /acls/{id}/delete", s.requireEdit(s.handleACLDelete))
	mux.HandleFunc("POST /rules", s.requireEdit(s.handleRuleSave))
	mux.HandleFunc("POST /rules/{id}/delete", s.requireEdit(s.handleRuleDelete))
	mux.HandleFunc("POST /rules/{id}/move", s.requireEdit(s.handleRuleMove))

	// Domains.
	mux.HandleFunc("GET /domains", s.requireAuth(s.handleDomains))
	mux.HandleFunc("GET /domains/new", s.requireEdit(s.handleDomainForm))
	mux.HandleFunc("GET /domains/{id}", s.requireAuth(s.handleDomainForm))
	mux.HandleFunc("POST /domains", s.requireEdit(s.handleDomainSave))
	mux.HandleFunc("POST /domains/{id}/delete", s.requireEdit(s.handleDomainDelete))

	// Certificates.
	mux.HandleFunc("GET /certificates", s.requireAuth(s.handleCerts))
	mux.HandleFunc("GET /certificates/new", s.requireEdit(s.handleCertForm))
	mux.HandleFunc("GET /certificates/{id}", s.requireAuth(s.handleCertForm))
	mux.HandleFunc("POST /certificates", s.requireEdit(s.handleCertSave))
	mux.HandleFunc("POST /certificates/{id}/delete", s.requireEdit(s.handleCertDelete))

	// Error pages.
	mux.HandleFunc("GET /error-pages", s.requireAuth(s.handleErrorPages))
	mux.HandleFunc("GET /error-pages/new", s.requireEdit(s.handleErrorPageForm))
	mux.HandleFunc("GET /error-pages/{id}", s.requireAuth(s.handleErrorPageForm))
	mux.HandleFunc("GET /error-pages/{id}/preview", s.requireAuth(s.handleErrorPagePreview))
	mux.HandleFunc("POST /error-pages", s.requireEdit(s.handleErrorPageSave))
	mux.HandleFunc("POST /error-pages/{id}/delete", s.requireEdit(s.handleErrorPageDelete))

	// Global, defaults and snippets.
	mux.HandleFunc("GET /global", s.requireAuth(s.handleGlobal))
	mux.HandleFunc("POST /global", s.requireEdit(s.handleGlobalSave))
	mux.HandleFunc("GET /defaults", s.requireAuth(s.handleDefaults))
	mux.HandleFunc("POST /defaults", s.requireEdit(s.handleDefaultsSave))
	mux.HandleFunc("POST /defaults/{id}/delete", s.requireEdit(s.handleDefaultsDelete))
	mux.HandleFunc("GET /snippets", s.requireAuth(s.handleSnippets))
	mux.HandleFunc("GET /snippets/new", s.requireEdit(s.handleSnippetForm))
	mux.HandleFunc("GET /snippets/{id}", s.requireAuth(s.handleSnippetForm))
	mux.HandleFunc("POST /snippets", s.requireEdit(s.handleSnippetSave))
	mux.HandleFunc("POST /snippets/{id}/delete", s.requireEdit(s.handleSnippetDelete))

	// Configuration lifecycle.
	mux.HandleFunc("GET /config", s.requireAuth(s.handleConfigPreview))
	mux.HandleFunc("GET /config/download", s.requireAuth(s.handleConfigDownload))
	mux.HandleFunc("POST /config/validate", s.requireEdit(s.handleValidate))
	mux.HandleFunc("POST /config/apply", s.requireEdit(s.handleApply))
	mux.HandleFunc("POST /config/reload", s.requireEdit(s.handleReload))
	mux.HandleFunc("POST /config/restart", s.requireAdmin(s.handleRestart))
	mux.HandleFunc("GET /versions", s.requireAuth(s.handleVersions))
	mux.HandleFunc("GET /versions/{id}", s.requireAuth(s.handleVersionDetail))
	mux.HandleFunc("POST /versions/{id}/rollback", s.requireEdit(s.handleRollback))

	// Assistant (AI agent).
	mux.HandleFunc("GET /assistant", s.requireEdit(s.handleAssistant))
	mux.HandleFunc("GET /assistant/{id}", s.requireEdit(s.handleAssistant))
	mux.HandleFunc("POST /assistant/new", s.requireEdit(s.handleAssistantNew))
	mux.HandleFunc("POST /assistant/{id}/message", s.requireEdit(s.handleAssistantMessage))
	mux.HandleFunc("POST /assistant/{id}/delete", s.requireEdit(s.handleAssistantDelete))

	// Assistant administration.
	mux.HandleFunc("GET /settings/ai", s.requireAdmin(s.handleAISettings))
	mux.HandleFunc("POST /settings/ai", s.requireAdmin(s.handleAISettingsSave))
	mux.HandleFunc("GET /settings/ai/models", s.requireAdmin(s.handleAIModels))
	mux.HandleFunc("POST /settings/ai/test", s.requireAdmin(s.handleAITest))

	// Administration.
	mux.HandleFunc("GET /users", s.requireAdmin(s.handleUsers))
	mux.HandleFunc("GET /users/new", s.requireAdmin(s.handleUserForm))
	mux.HandleFunc("GET /users/{id}", s.requireAdmin(s.handleUserForm))
	mux.HandleFunc("POST /users", s.requireAdmin(s.handleUserSave))
	mux.HandleFunc("POST /users/{id}/delete", s.requireAdmin(s.handleUserDelete))
	mux.HandleFunc("POST /users/{id}/unlock", s.requireAdmin(s.handleUserUnlock))
	mux.HandleFunc("GET /profile", s.requireAuth(s.handleProfile))
	mux.HandleFunc("POST /profile/password", s.requireAuth(s.handlePasswordChange))
	mux.HandleFunc("GET /settings", s.requireAdmin(s.handleSettings))
	mux.HandleFunc("POST /settings", s.requireAdmin(s.handleSettingsSave))
	mux.HandleFunc("GET /audit", s.requireAuth(s.handleAudit))

	return s.recoverPanic(s.securityHeaders(s.logRequests(mux)))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok"}`)
}

// newRenderer builds a renderer bound to the config's current paths. It is
// rebuilt whenever an administrator changes those paths from Settings.
func (s *Server) newRenderer() *hap.Renderer {
	return hap.NewRenderer(s.store, hap.Paths{
		ConfigPath:    s.cfg.ConfigPath,
		ErrorPagesDir: s.cfg.ErrorPagesDir,
		CertsDir:      s.cfg.CertsDir,
		MapsDir:       s.cfg.MapsDir,
	})
}
