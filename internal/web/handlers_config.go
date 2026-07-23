package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ebdaa/haproxy-controller/internal/hap"
	"github.com/ebdaa/haproxy-controller/internal/store"
)

// ---------------------------------------------------------------- dashboard

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	frontends, _ := s.store.ListFrontends(ctx)
	backends, _ := s.store.ListBackends(ctx)
	domains, _ := s.store.CountDomains(ctx)
	serverCount, _ := s.store.CountServers(ctx)
	certs, _ := s.store.ListCertificates(ctx)
	versions, _ := s.store.ListVersions(ctx, 5)
	recent, _ := s.store.ListAudit(ctx, "", 8, 0)

	enabledFrontends, enabledBackends := 0, 0
	for _, f := range frontends {
		if f.Enabled {
			enabledFrontends++
		}
	}
	for _, b := range backends {
		if b.Enabled {
			enabledBackends++
		}
	}

	var expiring []*store.Certificate
	for _, c := range certs {
		if c.IsExpired() || c.IsExpiringSoon() {
			expiring = append(expiring, c)
		}
	}

	statusText, running := s.deployer.Status(ctx)
	info, infoErr := s.runtime.ShowInfo(ctx)
	stats, statsErr := s.runtime.ShowStat(ctx)

	// Summarise server health across every backend.
	var upCount, downCount int
	for _, row := range stats {
		if row.Type == "server" {
			if row.IsUp() {
				upCount++
			} else if row.IsDown() {
				downCount++
			}
		}
	}

	version, _ := s.validator.Version(ctx)

	p := s.newPage(r, "Dashboard", "dashboard")
	p.Flash = s.takeFlash(w, r)
	p.Data["FrontendCount"] = len(frontends)
	p.Data["FrontendEnabled"] = enabledFrontends
	p.Data["BackendCount"] = len(backends)
	p.Data["BackendEnabled"] = enabledBackends
	p.Data["DomainCount"] = domains
	p.Data["ServerCount"] = serverCount
	p.Data["CertCount"] = len(certs)
	p.Data["ExpiringCerts"] = expiring
	p.Data["Versions"] = versions
	p.Data["Recent"] = recent
	p.Data["Running"] = running
	p.Data["StatusText"] = statusText
	p.Data["Info"] = info
	p.Data["InfoError"] = errText(infoErr)
	p.Data["Stats"] = stats
	p.Data["StatsError"] = errText(statsErr)
	p.Data["ServersUp"] = upCount
	p.Data["ServersDown"] = downCount
	p.Data["HAProxyVersion"] = version
	p.Data["ConfigPath"] = s.cfg.ConfigPath
	s.render(w, r, "dashboard.html", p)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, statsErr := s.runtime.ShowStat(ctx)
	info, infoErr := s.runtime.ShowInfo(ctx)
	statusText, running := s.deployer.Status(ctx)

	// Group server rows under their proxy so the table reads top-down.
	type group struct {
		Name     string
		Frontend *hap.StatRow
		Backend  *hap.StatRow
		Servers  []hap.StatRow
	}
	var groups []group
	index := map[string]int{}
	for _, row := range stats {
		idx, ok := index[row.ProxyName]
		if !ok {
			groups = append(groups, group{Name: row.ProxyName})
			idx = len(groups) - 1
			index[row.ProxyName] = idx
		}
		switch row.Type {
		case "frontend":
			cp := row
			groups[idx].Frontend = &cp
		case "backend":
			cp := row
			groups[idx].Backend = &cp
		case "server":
			groups[idx].Servers = append(groups[idx].Servers, row)
		}
	}

	p := s.newPage(r, "Live status", "status")
	p.Flash = s.takeFlash(w, r)
	p.Data["Groups"] = groups
	p.Data["Info"] = info
	p.Data["InfoError"] = errText(infoErr)
	p.Data["StatsError"] = errText(statsErr)
	p.Data["Running"] = running
	p.Data["StatusText"] = statusText
	p.Data["Socket"] = s.cfg.RuntimeSocket
	s.render(w, r, "status.html", p)
}

// handleStatusJSON backs the dashboard's live refresh.
func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.runtime.ShowStat(ctx)
	info, _ := s.runtime.ShowInfo(ctx)
	_, running := s.deployer.Status(ctx)

	payload := map[string]any{
		"running": running,
		"stats":   stats,
		"info":    info,
	}
	if err != nil {
		payload["error"] = err.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(payload)
}

// ----------------------------------------------------- configuration review

func (s *Server) handleConfigPreview(w http.ResponseWriter, r *http.Request) {
	res, checkOut, checkErr := s.deployer.Preview(r.Context())

	p := s.newPage(r, "Configuration", "config")
	p.Flash = s.takeFlash(w, r)
	if res != nil {
		p.Data["Content"] = res.Content
		p.Data["Warnings"] = res.Warnings
		p.Data["LineCount"] = strings.Count(res.Content, "\n")
	}
	p.Data["CheckOutput"] = checkOut
	p.Data["CheckError"] = errText(checkErr)
	p.Data["Valid"] = checkErr == nil
	p.Data["ConfigPath"] = s.cfg.ConfigPath
	s.render(w, r, "config.html", p)
}

func (s *Server) handleConfigDownload(w http.ResponseWriter, r *http.Request) {
	res, err := s.deployer.Renderer.Render(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="haproxy.cfg"`)
	_, _ = w.Write([]byte(res.Content))
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	_, out, err := s.deployer.Preview(r.Context())
	if err != nil {
		s.setFlash(w, r, "error", "The configuration did not pass validation.", out)
		redirect(w, r, "/config")
		return
	}
	s.audit(r, "config.validated", "config", "", "")
	s.setFlash(w, r, "success", "The configuration is valid.", out)
	redirect(w, r, "/config")
}

// handleApply is the Save & Apply action: render, validate, write, reload,
// with automatic rollback if HAProxy fails to come back up.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	comment := formStr(r, "comment")

	result, err := s.deployer.Apply(r.Context(), user.Username, comment)
	if err != nil {
		detail := result.CheckOutput
		if result.ReloadOutput != "" {
			detail = strings.TrimSpace(detail + "\n" + result.ReloadOutput)
		}
		s.audit(r, "config.apply_failed", "config", int64str(result.VersionID), err.Error())
		s.log.Error("apply failed", "user", user.Username, "error", err)

		msg := "Apply failed: " + err.Error()
		if result.RolledBack {
			msg = "Apply failed and the previous configuration was restored."
		}
		s.setFlash(w, r, "error", msg, detail)
		redirect(w, r, "/config")
		return
	}

	s.audit(r, "config.applied", "config", int64str(result.VersionID), comment)
	s.log.Info("configuration applied",
		"user", user.Username, "version", result.VersionID, "duration", result.Duration.String())

	detail := ""
	if len(result.Warnings) > 0 {
		detail = "Warnings:\n" + strings.Join(result.Warnings, "\n")
	}
	s.setFlash(w, r, "success",
		fmt.Sprintf("Configuration applied and HAProxy reloaded (version %d).", result.VersionID), detail)
	redirect(w, r, "/config")
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	out, err := s.deployer.Reload(r.Context())
	if err != nil {
		s.audit(r, "haproxy.reload_failed", "service", "", err.Error())
		s.setFlash(w, r, "error", "Reload failed: "+err.Error(), out)
		redirect(w, r, "/")
		return
	}
	s.audit(r, "haproxy.reloaded", "service", "", "")
	s.setFlash(w, r, "success", "HAProxy reloaded.", out)
	redirect(w, r, "/")
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	out, err := s.deployer.Restart(r.Context())
	if err != nil {
		s.audit(r, "haproxy.restart_failed", "service", "", err.Error())
		s.setFlash(w, r, "error", "Restart failed: "+err.Error(), out)
		redirect(w, r, "/")
		return
	}
	s.audit(r, "haproxy.restarted", "service", "", "")
	s.setFlash(w, r, "success", "HAProxy restarted.",
		"A restart drops existing connections; a reload does not.")
	redirect(w, r, "/")
}

// ----------------------------------------------------------------- versions

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := s.store.ListVersions(r.Context(), 100)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	p := s.newPage(r, "Version history", "versions")
	p.Flash = s.takeFlash(w, r)
	p.Data["Versions"] = versions
	s.render(w, r, "versions.html", p)
}

func (s *Server) handleVersionDetail(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That version")
		return
	}
	v, err := s.store.GetVersion(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That version")
		return
	}
	p := s.newPage(r, fmt.Sprintf("Version %d", v.ID), "versions")
	p.Flash = s.takeFlash(w, r)
	p.Data["Version"] = v
	s.render(w, r, "version_detail.html", p)
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That version")
		return
	}
	user := userFrom(r)

	result, err := s.deployer.Rollback(r.Context(), id, user.Username)
	if err != nil {
		detail := ""
		if result != nil {
			detail = strings.TrimSpace(result.CheckOutput + "\n" + result.ReloadOutput)
		}
		s.audit(r, "config.rollback_failed", "config", int64str(id), err.Error())
		s.setFlash(w, r, "error", "Rollback failed: "+err.Error(), detail)
		redirect(w, r, "/versions")
		return
	}

	s.audit(r, "config.rolled_back", "config", int64str(id), "")
	s.log.Warn("configuration rolled back", "user", user.Username, "version", id)
	s.setFlash(w, r, "success",
		fmt.Sprintf("Rolled back to version %d and reloaded HAProxy.", id), "")
	redirect(w, r, "/versions")
}

// -------------------------------------------------------------------- audit

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	filter := strings.TrimSpace(r.URL.Query().Get("q"))
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n := atoiSafe(v); n > 0 {
			page = n
		}
	}
	const perPage = 50
	offset := (page - 1) * perPage

	entries, err := s.store.ListAudit(r.Context(), filter, perPage, offset)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	total, _ := s.store.CountAudit(r.Context(), filter)

	p := s.newPage(r, "Audit log", "audit")
	p.Data["Entries"] = entries
	p.Data["Filter"] = filter
	p.Data["Page"] = page
	p.Data["Total"] = total
	p.Data["HasPrev"] = page > 1
	p.Data["HasNext"] = offset+len(entries) < total
	p.Data["PrevPage"] = page - 1
	p.Data["NextPage"] = page + 1
	s.render(w, r, "audit.html", p)
}

func atoiSafe(v string) int {
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			return 0
		}
	}
	return n
}
