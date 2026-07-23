package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

// loginDelay is applied to every failed attempt so that guessing usernames or
// passwords is uniformly slow regardless of which part was wrong.
const loginDelay = 400 * time.Millisecond

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.authenticate(r); ok {
		redirect(w, r, "/")
		return
	}
	p := s.newPage(r, "Sign in", "")
	p.Flash = s.takeFlash(w, r)
	p.Data["Next"] = safeNext(r.URL.Query().Get("next"))
	s.render(w, r, "login.html", p)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "The sign-in form could not be read.")
		return
	}

	username := formStr(r, "username")
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	ip := s.requestIP(r)

	fail := func(msg string) {
		time.Sleep(loginDelay)
		p := s.newPage(r, "Sign in", "")
		p.Flash = []flash{{Kind: "error", Message: msg}}
		p.Data["Next"] = next
		p.Data["Username"] = username
		s.render(w, r, "login.html", p)
	}

	if username == "" || password == "" {
		fail("Enter both a username and a password.")
		return
	}

	user, err := s.store.GetUserByUsername(r.Context(), username)
	if err != nil {
		s.log.Warn("failed login: unknown user", "username", username, "ip", ip)
		_ = s.store.Audit(r.Context(), store.AuditEntry{
			Username: username, Action: "login.failed", Detail: "unknown user", IP: ip,
		})
		// Deliberately the same message as a wrong password.
		fail("Incorrect username or password.")
		return
	}

	if !user.IsActive {
		s.log.Warn("failed login: disabled account", "username", username, "ip", ip)
		_ = s.store.Audit(r.Context(), store.AuditEntry{
			UserID: &user.ID, Username: user.Username,
			Action: "login.failed", Detail: "account disabled", IP: ip,
		})
		fail("This account is disabled. Contact an administrator.")
		return
	}

	if user.IsLocked() {
		mins := int(time.Until(*user.LockedUntil).Minutes()) + 1
		s.log.Warn("failed login: locked account", "username", username, "ip", ip)
		fail("Too many failed attempts. Try again in " + itoa(mins) + " minutes.")
		return
	}

	if !store.VerifyPassword(user.PasswordHash, password) {
		_ = s.store.RegisterFailedLogin(r.Context(), user.ID)
		s.log.Warn("failed login: bad password", "username", username, "ip", ip)
		_ = s.store.Audit(r.Context(), store.AuditEntry{
			UserID: &user.ID, Username: user.Username,
			Action: "login.failed", Detail: "bad password", IP: ip,
		})

		remaining := store.MaxFailedLogins - (user.FailedAttempts + 1)
		if remaining <= 0 {
			fail("Too many failed attempts. This account is locked for 15 minutes.")
			return
		}
		fail("Incorrect username or password.")
		return
	}

	// Credentials are good: issue a session.
	cookieValue, _, err := s.store.CreateSession(r.Context(), user.ID, ip, r.UserAgent(), s.cfg.SessionTTL())
	if err != nil {
		s.log.Error("create session", "error", err)
		fail("Could not start a session. Try again.")
		return
	}
	_ = s.store.RegisterSuccessfulLogin(r.Context(), user.ID, ip)
	_ = s.store.Audit(r.Context(), store.AuditEntry{
		UserID: &user.ID, Username: user.Username, Action: "login.success", IP: ip,
	})
	s.log.Info("login", "username", user.Username, "ip", ip)

	s.setSessionCookie(w, r, cookieValue)
	if user.MustChangePw {
		redirect(w, r, "/profile?first=1")
		return
	}
	redirect(w, r, next)
}

// setSessionCookie writes the session cookie with the strictest attributes the
// current transport allows.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string) {
	secure := r.TLS != nil
	if enabled, _, _ := s.cfg.TLS(); enabled {
		secure = true
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.SessionTTL().Seconds()),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess := sessionFrom(r); sess != nil {
		_ = s.store.DeleteSession(r.Context(), sess.ID)
	}
	if u := userFrom(r); u != nil {
		_ = s.store.Audit(r.Context(), store.AuditEntry{
			UserID: &u.ID, Username: u.Username, Action: "logout", IP: s.requestIP(r),
		})
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil,
	})
	redirect(w, r, "/login")
}

// safeNext keeps an open redirect out of the login flow by allowing only
// same-site absolute paths.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

// ------------------------------------------------------------------ profile

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	sessions, err := s.store.ListUserSessions(r.Context(), user.ID)
	if err != nil {
		s.log.Error("list sessions", "error", err)
	}

	p := s.newPage(r, "My profile", "profile")
	p.Flash = s.takeFlash(w, r)
	p.Data["Sessions"] = sessions
	p.Data["CurrentSession"] = sessionFrom(r)
	p.Data["FirstLogin"] = r.URL.Query().Get("first") == "1" || user.MustChangePw
	s.render(w, r, "profile.html", p)
}

func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")

	if !store.VerifyPassword(user.PasswordHash, current) {
		s.setFlash(w, r, "error", "Your current password is not correct.", "")
		redirect(w, r, "/profile")
		return
	}
	if next != confirm {
		s.setFlash(w, r, "error", "The new passwords do not match.", "")
		redirect(w, r, "/profile")
		return
	}
	if next == current {
		s.setFlash(w, r, "error", "The new password must differ from the current one.", "")
		redirect(w, r, "/profile")
		return
	}
	if err := s.store.SetPassword(r.Context(), user.ID, next); err != nil {
		s.setFlash(w, r, "error", err.Error(), "")
		redirect(w, r, "/profile")
		return
	}

	// Changing a password invalidates every other session for this account.
	sess := sessionFrom(r)
	if err := s.store.DeleteUserSessions(r.Context(), user.ID); err == nil && sess != nil {
		cookieValue, _, err := s.store.CreateSession(r.Context(), user.ID, s.requestIP(r), r.UserAgent(), s.cfg.SessionTTL())
		if err == nil {
			s.setSessionCookie(w, r, cookieValue)
		}
	}

	_ = s.store.Audit(r.Context(), store.AuditEntry{
		UserID: &user.ID, Username: user.Username,
		Action: "user.password_changed", Entity: "user", IP: s.requestIP(r),
	})
	s.setFlash(w, r, "success", "Your password has been changed. Other sessions were signed out.", "")
	redirect(w, r, "/profile")
}

func itoa(n int) string {
	if n < 1 {
		return "1"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
