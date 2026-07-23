package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

func int64str(v int64) string { return strconv.FormatInt(v, 10) }

// ------------------------------------------------------------------ domains

func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	domains, err := s.store.ListDomains(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	p := s.newPage(r, "Domains", "domains")
	p.Flash = s.takeFlash(w, r)
	p.Data["Domains"] = domains
	s.render(w, r, "domains.html", p)
}

func (s *Server) handleDomainForm(w http.ResponseWriter, r *http.Request) {
	d := &store.Domain{Enabled: true, MatchType: "exact", RedirectCode: 301}

	if raw := r.PathValue("id"); raw != "" {
		id, err := pathID(r, "id")
		if err != nil {
			s.notFound(w, r, "That domain")
			return
		}
		d, err = s.store.GetDomain(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r, "That domain")
				return
			}
			s.renderError(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}

	frontends, _ := s.store.ListFrontends(r.Context())
	backends, _ := s.store.ListBackends(r.Context())

	title := "New domain"
	if d.ID != 0 {
		title = "Domain: " + d.Hostname
	}
	p := s.newPage(r, title, "domains")
	p.Flash = s.takeFlash(w, r)
	p.Data["Domain"] = d
	p.Data["Frontends"] = frontends
	p.Data["Backends"] = backends
	p.Data["MatchTypes"] = store.MatchTypes
	s.render(w, r, "domain_edit.html", p)
}

func (s *Server) handleDomainSave(w http.ResponseWriter, r *http.Request) {
	d := &store.Domain{
		ID:           int64(formInt(r, "id", 0)),
		Hostname:     formStr(r, "hostname"),
		MatchType:    formStr(r, "match_type"),
		PathPrefix:   formStr(r, "path_prefix"),
		FrontendID:   int64(formInt(r, "frontend_id", 0)),
		BackendID:    formInt64Ptr(r, "backend_id"),
		RedirectTo:   formStr(r, "redirect_to"),
		RedirectCode: formInt(r, "redirect_code", 301),
		ForceHTTPS:   formBool(r, "force_https"),
		Enabled:      formBool(r, "enabled"),
		OrderIndex:   formInt(r, "order_index", 0),
	}

	back := "/domains/new"
	if d.ID != 0 {
		back = "/domains/" + int64str(d.ID)
	}
	if d.FrontendID == 0 {
		s.fail(w, r, back, errors.New("choose the frontend this domain arrives on"))
		return
	}

	id, err := s.store.SaveDomain(r.Context(), d)
	if err != nil {
		s.fail(w, r, back, err)
		return
	}

	action := "domain.created"
	if d.ID != 0 {
		action = "domain.updated"
	}
	s.audit(r, action, "domain", int64str(id), d.Hostname)
	s.setFlash(w, r, "success", "Domain "+d.Hostname+" saved. Apply the configuration to make it live.", "")
	redirect(w, r, "/domains")
}

func (s *Server) handleDomainDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That domain")
		return
	}
	d, err := s.store.GetDomain(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That domain")
		return
	}
	if err := s.store.DeleteDomain(r.Context(), id); err != nil {
		s.fail(w, r, "/domains", err)
		return
	}
	s.audit(r, "domain.deleted", "domain", int64str(id), d.Hostname)
	s.setFlash(w, r, "success", "Domain "+d.Hostname+" deleted.", "")
	redirect(w, r, "/domains")
}

// --------------------------------------------------------------- ACLs/rules

// ownerPath maps a scope and owner id back to its edit page.
func ownerPath(scope string, ownerID int64) string {
	if scope == "backend" {
		return "/backends/" + int64str(ownerID)
	}
	return "/frontends/" + int64str(ownerID)
}

func (s *Server) handleACLSave(w http.ResponseWriter, r *http.Request) {
	a := &store.ACL{
		ID:         int64(formInt(r, "acl_id", 0)),
		Scope:      formStr(r, "scope"),
		OwnerID:    int64(formInt(r, "owner_id", 0)),
		Name:       formStr(r, "name"),
		Expression: formStr(r, "expression"),
		Enabled:    formBool(r, "enabled"),
		OrderIndex: formInt(r, "order_index", 0),
	}
	if a.Scope != "frontend" && a.Scope != "backend" {
		s.renderError(w, r, http.StatusBadRequest, "Unknown ACL scope.")
		return
	}
	back := ownerPath(a.Scope, a.OwnerID)

	if _, err := s.store.SaveACL(r.Context(), a); err != nil {
		s.fail(w, r, back, err)
		return
	}
	s.audit(r, "acl.saved", a.Scope, int64str(a.OwnerID), a.Name)
	s.setFlash(w, r, "success", "ACL "+a.Name+" saved.", "")
	redirect(w, r, back)
}

func (s *Server) handleACLDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That ACL")
		return
	}
	a, err := s.store.GetACL(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That ACL")
		return
	}
	back := ownerPath(a.Scope, a.OwnerID)
	if err := s.store.DeleteACL(r.Context(), id); err != nil {
		s.fail(w, r, back, err)
		return
	}
	s.audit(r, "acl.deleted", a.Scope, int64str(a.OwnerID), a.Name)
	s.setFlash(w, r, "success", "ACL "+a.Name+" removed.", "")
	redirect(w, r, back)
}

func (s *Server) handleRuleSave(w http.ResponseWriter, r *http.Request) {
	rule := &store.Rule{
		ID:         int64(formInt(r, "rule_id", 0)),
		Scope:      formStr(r, "scope"),
		OwnerID:    int64(formInt(r, "owner_id", 0)),
		Directive:  formStr(r, "directive"),
		Argument:   formStr(r, "argument"),
		Condition:  formStr(r, "condition"),
		Enabled:    formBool(r, "enabled"),
		OrderIndex: formInt(r, "order_index", 0),
	}
	if rule.Scope != "frontend" && rule.Scope != "backend" {
		s.renderError(w, r, http.StatusBadRequest, "Unknown rule scope.")
		return
	}
	back := ownerPath(rule.Scope, rule.OwnerID)

	if _, err := s.store.SaveRule(r.Context(), rule); err != nil {
		s.fail(w, r, back, err)
		return
	}
	s.audit(r, "rule.saved", rule.Scope, int64str(rule.OwnerID), rule.Directive+" "+rule.Argument)
	s.setFlash(w, r, "success", "Rule saved.", "")
	redirect(w, r, back)
}

func (s *Server) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That rule")
		return
	}
	rule, err := s.store.GetRule(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That rule")
		return
	}
	back := ownerPath(rule.Scope, rule.OwnerID)
	if err := s.store.DeleteRule(r.Context(), id); err != nil {
		s.fail(w, r, back, err)
		return
	}
	s.audit(r, "rule.deleted", rule.Scope, int64str(rule.OwnerID), rule.Directive)
	s.setFlash(w, r, "success", "Rule removed.", "")
	redirect(w, r, back)
}

// handleRuleMove reorders a rule, which matters because HAProxy evaluates
// these directives strictly in file order.
func (s *Server) handleRuleMove(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That rule")
		return
	}
	rule, err := s.store.GetRule(r.Context(), id)
	if err != nil {
		s.notFound(w, r, "That rule")
		return
	}
	delta := -1
	if formStr(r, "direction") == "down" {
		delta = 1
	}
	back := ownerPath(rule.Scope, rule.OwnerID)
	if err := s.store.MoveRule(r.Context(), id, delta); err != nil {
		s.fail(w, r, back, err)
		return
	}
	redirect(w, r, back)
}
