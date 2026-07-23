package hap

import (
	"strings"
	"testing"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

func TestHostACLLines(t *testing.T) {
	tests := []struct {
		name   string
		domain store.Domain
		want   []string
	}{
		{
			name:   "exact matches only that host",
			domain: store.Domain{Hostname: "example.com", MatchType: "exact"},
			want:   []string{"-m str example.com"},
		},
		{
			name:   "subdomain matches the host and below",
			domain: store.Domain{Hostname: "example.com", MatchType: "subdomain"},
			want:   []string{"-m str example.com", "-m end .example.com"},
		},
		{
			name:   "wildcard strips the star and matches only below",
			domain: store.Domain{Hostname: "*.example.com", MatchType: "wildcard"},
			want:   []string{"-m end .example.com"},
		},
		{
			name:   "regex is passed through",
			domain: store.Domain{Hostname: "^app[0-9]+\\.example\\.com$", MatchType: "regex"},
			want:   []string{"-m reg ^app[0-9]+\\.example\\.com$"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hostACLLines("h", tc.domain)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines, want %d: %v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("line %d = %q, want it to contain %q", i, got[i], want)
				}
				if !strings.HasPrefix(got[i], "acl h ") {
					t.Errorf("line %d = %q, want an acl declaration", i, got[i])
				}
			}
		})
	}
}

// The host expression must drop the port, or "example.com:8443" would fail an
// exact match against "example.com".
func TestHostExprStripsPort(t *testing.T) {
	if !strings.Contains(hostExpr, "field(1,:)") {
		t.Errorf("hostExpr = %q, want it to strip the port", hostExpr)
	}
	if !strings.Contains(hostExpr, "lower") {
		t.Errorf("hostExpr = %q, want it to lowercase the host", hostExpr)
	}
}

func TestBindListen(t *testing.T) {
	tests := []struct {
		bind store.Bind
		want string
	}{
		{store.Bind{Address: "*", Port: 443}, ":443"},
		{store.Bind{Address: "", Port: 80}, ":80"},
		{store.Bind{Address: "10.0.0.1", Port: 8080}, "10.0.0.1:8080"},
		{store.Bind{Address: "::1", Port: 8080}, "[::1]:8080"},
		{store.Bind{Address: "[::1]", Port: 8080}, "[::1]:8080"},
	}
	for _, tc := range tests {
		if got := tc.bind.Listen(); got != tc.want {
			t.Errorf("Bind{%q, %d}.Listen() = %q, want %q", tc.bind.Address, tc.bind.Port, got, tc.want)
		}
	}
}

func TestServerLine(t *testing.T) {
	bk := &store.Backend{Name: "app", CookieName: "SRVID"}

	sv := store.Server{
		Name: "web1", Address: "10.0.0.11", Port: 8080, Weight: 100,
		CheckEnabled: true, CheckInter: "2s", CheckRise: 2, CheckFall: 3,
		CookieValue: "s1",
	}
	got := serverLine(bk, sv)
	for _, want := range []string{"server web1 10.0.0.11:8080", "check", "inter 2s", "rise 2", "fall 3", "cookie s1"} {
		if !strings.Contains(got, want) {
			t.Errorf("serverLine() = %q, want it to contain %q", got, want)
		}
	}
	// A weight of 100 is HAProxy's default and should stay implicit.
	if strings.Contains(got, "weight") {
		t.Errorf("serverLine() = %q, want the default weight omitted", got)
	}

	// A cookie value is meaningless without a cookie name on the backend.
	noCookie := serverLine(&store.Backend{Name: "app"}, sv)
	if strings.Contains(noCookie, "cookie") {
		t.Errorf("serverLine() = %q, want no cookie when the backend sets none", noCookie)
	}

	// IPv6 literals need brackets before the port is appended.
	v6 := serverLine(bk, store.Server{Name: "w", Address: "fd00::1", Port: 443, Weight: 100})
	if !strings.Contains(v6, "[fd00::1]:443") {
		t.Errorf("serverLine() = %q, want a bracketed IPv6 address", v6)
	}
}

func TestRenderRule(t *testing.T) {
	got := renderRule(store.Rule{Directive: "http-request", Argument: "deny", Condition: "if bad"})
	if got != "http-request deny if bad" {
		t.Errorf("renderRule() = %q", got)
	}
	got = renderRule(store.Rule{Directive: "option", Argument: "httpclose"})
	if got != "option httpclose" {
		t.Errorf("renderRule() = %q", got)
	}
}

func TestQuoteIfNeeded(t *testing.T) {
	if got := quoteIfNeeded("/plain"); got != "/plain" {
		t.Errorf("quoteIfNeeded(/plain) = %q", got)
	}
	if got := quoteIfNeeded("has space"); got != `"has space"` {
		t.Errorf("quoteIfNeeded(has space) = %q", got)
	}
}

// raw() must indent operator text to section depth so it lands inside the
// section it was written for.
func TestBufRawIndents(t *testing.T) {
	b := &buf{}
	b.raw("option httpclose\n\n  # a comment\n")
	got := b.String()
	if !strings.Contains(got, "    option httpclose\n") {
		t.Errorf("raw() = %q, want the directive indented", got)
	}
	if !strings.Contains(got, "    # a comment\n") {
		t.Errorf("raw() = %q, want the comment indented", got)
	}
	if strings.Contains(got, "\n\n") {
		t.Errorf("raw() = %q, want blank lines dropped", got)
	}
}
