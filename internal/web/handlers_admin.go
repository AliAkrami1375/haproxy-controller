package web

import (
	"errors"
	"net/http"

	"github.com/ebdaa/haproxy-controller/internal/hap"
	"github.com/ebdaa/haproxy-controller/internal/store"
)

// -------------------------------------------------------------------- users

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	p := s.newPage(r, "Users", "users")
	p.Flash = s.takeFlash(w, r)
	p.Data["Users"] = users
	s.render(w, r, "users.html", p)
}

func (s *Server) handleUserForm(w http.ResponseWriter, r *http.Request) {
	u := &store.User{Role: store.RoleOperator, IsActive: true, MustChangePw: true}

	if raw := r.PathValue("id"); raw != "" {
		id, err := pathID(r, "id")
		if err != nil {
			s.notFound(w, r, "That user")
			return
		}
		u, err = s.store.GetUser(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r, "That user")
				return
			}
			s.renderError(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}

	title := "New user"
	if u.ID != 0 {
		title = "User: " + u.Username
	}
	p := s.newPage(r, title, "users")
	p.Flash = s.takeFlash(w, r)
	p.Data["Account"] = u
	p.Data["Roles"] = []string{store.RoleAdmin, store.RoleOperator, store.RoleViewer}
	s.render(w, r, "user_edit.html", p)
}

func (s *Server) handleUserSave(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r)
	id := int64(formInt(r, "id", 0))
	password := r.PostFormValue("password")

	u := &store.User{
		ID:           id,
		Username:     formStr(r, "username"),
		FullName:     formStr(r, "full_name"),
		Email:        formStr(r, "email"),
		Role:         formStr(r, "role"),
		IsActive:     formBool(r, "is_active"),
		MustChangePw: formBool(r, "must_change_pw"),
	}

	back := "/users/new"
	if id != 0 {
		back = "/users/" + int64str(id)
	}

	if id == 0 {
		newID, err := s.store.CreateUser(r.Context(), u, password)
		if err != nil {
			s.fail(w, r, back, err)
			return
		}
		s.audit(r, "user.created", "user", int64str(newID), u.Username+" as "+u.Role)
		s.setFlash(w, r, "success", "User "+u.Username+" created.", "")
		redirect(w, r, "/users")
		return
	}

	existing, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That user")
		return
	}
	// The username is the account's identity; changing it is not supported.
	u.Username = existing.Username

	// Never let the last administrator lose access, including to themselves.
	if existing.IsAdmin() && (!u.IsAdmin() || !u.IsActive) {
		admins, _ := s.store.CountAdmins(r.Context())
		if admins <= 1 {
			s.fail(w, r, back, errors.New("this is the last active administrator; promote another account first"))
			return
		}
	}
	if actor.ID == id && !u.IsActive {
		s.fail(w, r, back, errors.New("you cannot disable your own account"))
		return
	}

	if err := s.store.UpdateUser(r.Context(), u); err != nil {
		s.fail(w, r, back, err)
		return
	}

	if password != "" {
		if err := s.store.SetPassword(r.Context(), id, password); err != nil {
			s.fail(w, r, back, err)
			return
		}
		// A password reset must invalidate that user's existing sessions.
		_ = s.store.DeleteUserSessions(r.Context(), id)
		s.audit(r, "user.password_reset", "user", int64str(id), u.Username)
	}

	// A role change or deactivation takes effect immediately.
	if existing.Role != u.Role || (existing.IsActive && !u.IsActive) {
		_ = s.store.DeleteUserSessions(r.Context(), id)
	}

	s.audit(r, "user.updated", "user", int64str(id), u.Username+" as "+u.Role)
	s.setFlash(w, r, "success", "User "+u.Username+" updated.", "")
	redirect(w, r, "/users")
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That user")
		return
	}
	actor := userFrom(r)
	if actor.ID == id {
		s.fail(w, r, "/users", errors.New("you cannot delete your own account"))
		return
	}

	u, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That user")
		return
	}
	if u.IsAdmin() {
		admins, _ := s.store.CountAdmins(r.Context())
		if admins <= 1 {
			s.fail(w, r, "/users", errors.New("this is the last active administrator and cannot be deleted"))
			return
		}
	}

	if err := s.store.DeleteUser(r.Context(), id); err != nil {
		s.fail(w, r, "/users", err)
		return
	}
	s.audit(r, "user.deleted", "user", int64str(id), u.Username)
	s.setFlash(w, r, "success", "User "+u.Username+" deleted.", "")
	redirect(w, r, "/users")
}

func (s *Server) handleUserUnlock(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That user")
		return
	}
	u, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That user")
		return
	}
	if err := s.store.UnlockUser(r.Context(), id); err != nil {
		s.fail(w, r, "/users", err)
		return
	}
	s.audit(r, "user.unlocked", "user", int64str(id), u.Username)
	s.setFlash(w, r, "success", "User "+u.Username+" unlocked.", "")
	redirect(w, r, "/users")
}

// ----------------------------------------------------------------- settings

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	version, versionErr := s.validator.Version(ctx)

	p := s.newPage(r, "Settings", "settings")
	p.Flash = s.takeFlash(w, r)
	p.Data["Config"] = s.cfg
	p.Data["ConfigFile"] = s.cfg.Path()
	p.Data["HAProxyVersion"] = version
	p.Data["HAProxyError"] = errText(versionErr)
	p.Data["SocketOK"] = s.runtime.Available(ctx)
	p.Data["ProcessMode"] = s.deployer.Process.Kind()
	p.Data["Supervised"] = s.deployer.Process.Kind() == hap.KindSupervised
	p.Data["KeepVersions"] = s.store.GetSettingInt(ctx, store.SetAutoBackupKeep, 30)
	p.Data["PanelTitle"] = s.store.GetSetting(ctx, store.SetPanelTitle, appName)
	s.render(w, r, "settings.html", p)
}

// handleSettingsSave persists controller settings. Changing the listen
// address, port or TLS material rebinds the listener in place, so the
// operator only has to follow the new URL.
func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Panel preferences.
	if err := s.store.SetSetting(ctx, store.SetPanelTitle, formStr(r, "panel_title")); err != nil {
		s.fail(w, r, "/settings", err)
		return
	}
	keep := formInt(r, "keep_versions", 30)
	if keep < 5 {
		keep = 5
	}
	_ = s.store.SetSetting(ctx, store.SetAutoBackupKeep, int64str(int64(keep)))

	// HAProxy integration paths and commands.
	s.cfg.HAProxyBin = formStr(r, "haproxy_bin")
	s.cfg.ConfigPath = formStr(r, "config_path")
	s.cfg.ErrorPagesDir = formStr(r, "error_pages_dir")
	s.cfg.CertsDir = formStr(r, "certs_dir")
	s.cfg.RuntimeSocket = formStr(r, "runtime_socket")
	if s.deployer.Process.Kind() == hap.KindCommand {
		s.cfg.ReloadCommand = formStr(r, "reload_command")
		s.cfg.RestartCommand = formStr(r, "restart_command")
		s.cfg.StatusCommand = formStr(r, "status_command")
	}
	s.cfg.SessionTTLMins = formInt(r, "session_ttl_minutes", 720)

	// Push the new paths into the live components.
	s.validator.Binary = s.cfg.HAProxyBin
	s.runtime.Socket = s.cfg.RuntimeSocket
	s.deployer.ConfigPath = s.cfg.ConfigPath
	s.deployer.ErrorPagesDir = s.cfg.ErrorPagesDir
	s.deployer.CertsDir = s.cfg.CertsDir
	// Reload/restart/status commands only apply when the controller drives
	// HAProxy through an init system. Under supervision they are ignored,
	// so the form does not offer them.
	if cm, ok := s.deployer.Process.(*hap.CommandManager); ok {
		cm.ReloadCommand = s.cfg.ReloadCommand
		cm.RestartCommand = s.cfg.RestartCommand
		cm.StatusCommand = s.cfg.StatusCommand
	}
	// The renderer embeds the paths it writes into the config, so rebuild it.
	s.deployer.Renderer = s.newRenderer()

	// Listener: only rebind when something actually changed.
	newAddr := formStr(r, "listen_addr")
	newPort := formInt(r, "listen_port", s.cfg.ListenPort)
	tlsEnabled := formBool(r, "tls_enabled")
	tlsCert := formStr(r, "tls_cert")
	tlsKey := formStr(r, "tls_key")

	oldAddr, oldPort := s.cfg.ListenAddr, s.cfg.ListenPort
	oldTLS, oldCert, oldKey := s.cfg.TLS()
	listenerChanged := newAddr != oldAddr || newPort != oldPort ||
		tlsEnabled != oldTLS || tlsCert != oldCert || tlsKey != oldKey

	// Refuse a port already claimed by an enabled HAProxy frontend.
	if listenerChanged {
		if inUse, err := s.store.PortsInUse(ctx); err == nil {
			for key, frontend := range inUse {
				addr, portStr, _ := cutLast(key, ":")
				if atoiSafe(portStr) == newPort && addressOverlaps(addr, newAddr) {
					s.fail(w, r, "/settings", errors.New(
						"port "+itoa(newPort)+" is already used by frontend "+frontend))
					return
				}
			}
		}
	}

	if err := s.cfg.UpdateListener(newAddr, newPort, tlsEnabled, tlsCert, tlsKey); err != nil {
		s.fail(w, r, "/settings", err)
		return
	}
	if err := s.cfg.Save(); err != nil {
		s.fail(w, r, "/settings", err)
		return
	}

	s.audit(r, "settings.updated", "settings", "", "")

	if listenerChanged {
		scheme := "http"
		if tlsEnabled {
			scheme = "https"
		}
		s.setFlash(w, r, "success", "Settings saved. The control panel is moving to a new address.",
			"Continue at "+scheme+"://"+hostOf(r)+":"+itoa(newPort)+"/")
		redirect(w, r, "/settings")
		// Rebind after the response has been handed to the client.
		go s.Rebind()
		return
	}

	s.setFlash(w, r, "success", "Settings saved.", "")
	redirect(w, r, "/settings")
}

// cutLast splits on the final occurrence of sep, which is what an
// "address:port" key needs when the address may be an IPv6 literal.
func cutLast(s, sep string) (before, after string, found bool) {
	for i := len(s) - len(sep); i >= 0; i-- {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

// hostOf returns the request host without its port.
func hostOf(r *http.Request) string {
	host, _, found := cutLast(r.Host, ":")
	if !found {
		return r.Host
	}
	return host
}
