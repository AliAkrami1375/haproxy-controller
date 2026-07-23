package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
)

// sessionCookieName is prefixed with __Host- when served over TLS, which pins
// the cookie to this exact host and path.
const (
	cookieName    = "hc_session"
	cookieNameTLS = "__Host-hc_session"
)

// maxUploadBytes caps multipart request bodies held in memory. The only
// uploads the panel accepts are PEM files, which are a few KiB.
const maxUploadBytes = 8 << 20

func (s *Server) cookieName() string {
	if enabled, _, _ := s.cfg.TLS(); enabled {
		return cookieNameTLS
	}
	return cookieName
}

// userFrom returns the authenticated user attached to a request, if any.
func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxUser).(*store.User)
	return u
}

// sessionFrom returns the session attached to a request, if any.
func sessionFrom(r *http.Request) *store.Session {
	sess, _ := r.Context().Value(ctxSession).(*store.Session)
	return sess
}

// authenticate resolves the session cookie into a user.
func (s *Server) authenticate(r *http.Request) (*store.Session, *store.User, bool) {
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return nil, nil, false
	}
	sess, user, err := s.store.LookupSession(r.Context(), c.Value)
	if err != nil {
		return nil, nil, false
	}
	return sess, user, true
}

// requireAuth enforces a valid session and CSRF token on unsafe methods.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, user, ok := s.authenticate(r)
		if !ok {
			s.redirectToLogin(w, r)
			return
		}
		if !s.enforceCSRF(w, r, sess) {
			return
		}

		// Slide the expiry forward on activity, at most once a minute.
		if time.Since(sess.LastSeenAt) > time.Minute {
			_ = s.store.TouchSession(r.Context(), sess.ID, s.cfg.SessionTTL())
		}

		// A forced password change blocks everything but the profile page.
		if user.MustChangePw && !strings.HasPrefix(r.URL.Path, "/profile") && r.URL.Path != "/logout" {
			http.Redirect(w, r, "/profile?first=1", http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), ctxUser, user)
		ctx = context.WithValue(ctx, ctxSession, sess)
		next(w, r.WithContext(ctx))
	}
}

// requireEdit additionally rejects read-only accounts.
func (s *Server) requireEdit(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if u := userFrom(r); u == nil || !u.CanEdit() {
			s.renderError(w, r, http.StatusForbidden,
				"Your account has read-only access to this panel.")
			return
		}
		next(w, r)
	})
}

// requireAdmin restricts a route to administrators.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if u := userFrom(r); u == nil || !u.IsAdmin() {
			s.renderError(w, r, http.StatusForbidden,
				"This section is restricted to administrators.")
			return
		}
		next(w, r)
	})
}

// enforceCSRF validates the token on state-changing requests and checks that
// the request originated from this panel.
func (s *Server) enforceCSRF(w http.ResponseWriter, r *http.Request, sess *store.Session) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}

	// A cross-site form post carries an Origin the browser sets itself, so it
	// cannot be forged by the attacking page.
	if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(r, origin) {
		s.renderError(w, r, http.StatusForbidden, "This request came from another site and was refused.")
		return false
	}

	// Multipart bodies (certificate uploads) are not parsed by ParseForm, and
	// PostFormValue will not fall back once PostForm is non-nil. Parse the
	// right shape up front so the token is visible either way.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
			s.renderError(w, r, http.StatusBadRequest,
				"The upload could not be read. It may be larger than the 8 MiB limit.")
			return false
		}
	} else if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "The form could not be read.")
		return false
	}
	token := r.PostFormValue("csrf_token")
	if token == "" {
		token = r.Header.Get("X-CSRF-Token")
	}
	if !store.ConstantTimeEqual(token, sess.CSRFToken) {
		s.renderError(w, r, http.StatusForbidden,
			"Your session token has expired. Reload the page and try again.")
		return false
	}
	return true
}

// sameOrigin compares an Origin header against the request's own host.
func (s *Server) sameOrigin(r *http.Request, origin string) bool {
	i := strings.Index(origin, "://")
	if i < 0 {
		return false
	}
	return strings.EqualFold(origin[i+3:], r.Host)
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.RequestURI()
	if r.Method != http.MethodGet || next == "/login" {
		next = "/"
	}
	http.Redirect(w, r, "/login?next="+urlQueryEscape(next), http.StatusSeeOther)
}

// securityHeaders applies a strict, self-contained policy. The panel loads no
// third-party assets, so the CSP can forbid them outright.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		if enabled, _, _ := s.cfg.TLS(); enabled {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		s.log.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"ip", clientIP(r),
			"duration", time.Since(start).String())
	})
}

// recoverPanic keeps one bad handler from taking down the control panel.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request",
					"path", r.URL.Path, "error", rec, "stack", string(debug.Stack()))
				s.renderError(w, r, http.StatusInternalServerError,
					"The control panel hit an unexpected error. The details are in the service log.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the peer address, honouring X-Forwarded-For only when the
// immediate peer is a configured trusted proxy.
func clientIPWithTrust(r *http.Request, trusted []string) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if len(trusted) == 0 {
		return peer
	}
	if !isTrusted(peer, trusted) {
		return peer
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// The left-most entry is the original client.
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	return peer
}

func isTrusted(ip string, trusted []string) bool {
	parsed := net.ParseIP(ip)
	for _, t := range trusted {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if strings.Contains(t, "/") {
			if _, cidr, err := net.ParseCIDR(t); err == nil && parsed != nil && cidr.Contains(parsed) {
				return true
			}
			continue
		}
		if t == ip {
			return true
		}
	}
	return false
}

// clientIP is the package-level helper used by handlers; the trusted-proxy
// list is applied by the Server wrapper below.
func clientIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return peer
}

// requestIP resolves the client address using the configured trusted proxies.
func (s *Server) requestIP(r *http.Request) string {
	return clientIPWithTrust(r, s.cfg.TrustedProxies)
}

// errForbidden is returned by helpers that refuse an action on policy grounds.
var errForbidden = errors.New("forbidden")
