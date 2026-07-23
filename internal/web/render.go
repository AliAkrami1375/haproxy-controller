package web

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

// Brand values shown in the panel chrome.
const (
	brandName  = store.BrandName
	brandURL   = store.BrandURL
	appName    = "HAProxy Controller"
	appTagline = "HAProxy configuration, in one place"
)

// pageData is the root context every template receives.
type pageData struct {
	Title     string
	Active    string
	User      *store.User
	CSRFToken string
	Flash     []flash
	Brand     brand
	Version   string
	Data      map[string]any
}

type brand struct {
	Name    string
	URL     string
	App     string
	Tagline string
}

// flash is a one-shot message rendered at the top of a page.
type flash struct {
	Kind    string // success | error | warning | info
	Message string
	Detail  string
}

// parseTemplates compiles the embedded template set with the helper funcs.
func parseTemplates() (*template.Template, error) {
	return template.New("").Funcs(templateFuncs()).ParseFS(templateFS, "templates/*.html")
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"yesno": func(b bool) string {
			if b {
				return "Yes"
			}
			return "No"
		},
		"checked": func(b bool) template.HTMLAttr {
			if b {
				return template.HTMLAttr("checked")
			}
			return ""
		},
		"selected": func(a, b any) template.HTMLAttr {
			if fmt.Sprint(a) == fmt.Sprint(b) {
				return template.HTMLAttr("selected")
			}
			return ""
		},
		"eqi":   func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) },
		"join":  strings.Join,
		"lower": strings.ToLower,
		"date":  formatDate,
		"ago":   humanizeAgo,
		"bytes": humanizeBytes,
		"num":   humanizeNumber,
		"dict": func(values ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(values); i += 2 {
				m[fmt.Sprint(values[i])] = values[i+1]
			}
			return m
		},
		"deref": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"statusText": store.StatusText,
		"mdInline":   mdInline,
		// short renders the first 12 characters of a hash for compact display.
		"short": func(v string) string {
			if len(v) <= 12 {
				return v
			}
			return v[:12]
		},
		"add": func(a, b int) int { return a + b },
	}
}

// asTime accepts either a time.Time or a *time.Time, because several model
// fields are optional pointers that are nil until the event first happens.
// Templates dereference pointers automatically, which panics on nil, so the
// helpers must take the value untyped and unwrap it themselves.
func asTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case *time.Time:
		if t == nil {
			return time.Time{}
		}
		return *t
	}
	return time.Time{}
}

// formatDate renders an absolute timestamp, or "never" when unset.
func formatDate(v any) string {
	t := asTime(v)
	if t.IsZero() {
		return "never"
	}
	return t.Format("2006-01-02 15:04 MST")
}

func humanizeAgo(v any) string {
	t := asTime(v)
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}

func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func humanizeNumber(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// newPage builds the base template context for a request.
func (s *Server) newPage(r *http.Request, title, active string) *pageData {
	p := &pageData{
		Title:  title,
		Active: active,
		User:   userFrom(r),
		Brand:  brand{Name: brandName, URL: brandURL, App: appName, Tagline: appTagline},
		Data:   map[string]any{},
	}
	if sess := sessionFrom(r); sess != nil {
		p.CSRFToken = sess.CSRFToken
	}
	return p
}

// render writes a template, buffering first so a template error cannot emit a
// half-written page with a 200 status.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data *pageData) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render template", "template", name, "error", err)
		http.Error(w, "Template error. See the service log for details.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
}

// renderError shows a styled error page at the given status.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	p := s.newPage(r, http.StatusText(status), "")
	p.Data["Status"] = status
	p.Data["Message"] = message

	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, "error.html", p); err != nil {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// flashCookie carries a one-shot message across a redirect.
const flashCookie = "hc_flash"

// setFlash stores a message to show after the next redirect.
func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, kind, message, detail string) {
	v := url.Values{}
	v.Set("k", kind)
	v.Set("m", message)
	if detail != "" {
		v.Set("d", truncate(detail, 1500))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    url.QueryEscape(v.Encode()),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   30,
	})
}

// takeFlash reads and clears any pending flash message.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) []flash {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil,
	})

	raw, err := url.QueryUnescape(c.Value)
	if err != nil {
		return nil
	}
	v, err := url.ParseQuery(raw)
	if err != nil {
		return nil
	}
	kind := v.Get("k")
	if kind == "" {
		kind = "info"
	}
	return []flash{{Kind: kind, Message: v.Get("m"), Detail: v.Get("d")}}
}

// redirect issues a see-other, the correct response after a successful POST.
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// pathID reads a numeric path parameter.
func pathID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return id, nil
}

// formInt reads an integer form field, falling back to def when absent.
func formInt(r *http.Request, name string, def int) int {
	raw := strings.TrimSpace(r.PostFormValue(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// formInt64Ptr reads an optional foreign key, returning nil when unset.
func formInt64Ptr(r *http.Request, name string) *int64 {
	raw := strings.TrimSpace(r.PostFormValue(name))
	if raw == "" || raw == "0" {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

// formBool reads a checkbox.
func formBool(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.PostFormValue(name))) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

// formStr reads and trims a text field.
func formStr(r *http.Request, name string) string {
	return strings.TrimSpace(r.PostFormValue(name))
}

// normalizeLines trims each line of a textarea and drops empty ones, keeping
// stored multi-line fields tidy.
func normalizeLines(v string) string {
	return strings.Join(store.SplitLines(v), "\n")
}

// normalizeBlock only normalises line endings, preserving indentation and
// blank lines, which matters for snippets and HTML bodies.
func normalizeBlock(v string) string {
	v = strings.ReplaceAll(v, "\r\n", "\n")
	return strings.TrimRight(v, " \t\n")
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// mdInlineRe matches `code` spans and **bold** runs in assistant replies.
var (
	mdCodeRe = regexp.MustCompile("`([^`]+)`")
	mdBoldRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
)

// mdInline renders a small, safe subset of Markdown (inline code and bold) for
// assistant messages. Everything is HTML-escaped first, so the model's text can
// never inject markup.
func mdInline(text string) template.HTML {
	escaped := template.HTMLEscapeString(text)
	escaped = mdCodeRe.ReplaceAllString(escaped, "<code>$1</code>")
	escaped = mdBoldRe.ReplaceAllString(escaped, "<strong>$1</strong>")
	return template.HTML(escaped)
}
