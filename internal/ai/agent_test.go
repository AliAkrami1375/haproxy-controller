package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ebdaa/haproxy-controller/internal/db"
	"github.com/ebdaa/haproxy-controller/internal/store"
)

// stubPreviewer reports the configuration as valid, standing in for a real
// HAProxy render+check in the agent loop.
type stubPreviewer struct{ calls int }

func (s *stubPreviewer) PreviewConfig(context.Context) (string, []string, string, error) {
	s.calls++
	return "global\n\ndefaults\n\nfrontend public\n    bind :443\n", nil, "Configuration file is valid", nil
}

// scriptedModel drives the agent through a fixed sequence of tool calls, then a
// final message, mimicking how a real model would build a site. It lets the
// test exercise the whole loop deterministically, with no network or API key.
func scriptedModel(t *testing.T) *httptest.Server {
	step := 0
	call := func(id, name, args string) map[string]any {
		return map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": id, "type": "function",
						"function": map[string]any{"name": name, "arguments": args},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"total_tokens": 100},
		}
	}
	final := func(text string) map[string]any {
		return map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"total_tokens": 50},
		}
	}

	// The scripted plan: understand → build backend+server → build frontend+bind
	// → route → validate → summarise.
	plan := []map[string]any{
		call("c1", "get_overview", `{}`),
		call("c2", "create_backend", `{"name":"web_pool","mode":"http","balance":"roundrobin","health_check":true,"check_uri":"/healthz"}`),
		call("c3", "add_server", `{"backend":"web_pool","name":"web1","address":"10.0.0.11","port":8080}`),
		call("c4", "create_frontend", `{"name":"public","mode":"http","default_backend":"web_pool","force_https":true,"enable_hsts":true}`),
		call("c5", "add_bind", `{"frontend":"public","address":"*","port":443,"ssl":true}`),
		call("c6", "add_route", `{"frontend":"public","hostname":"www.example.com","match_type":"subdomain","backend":"web_pool"}`),
		call("c7", "validate_config", `{}`),
		final("I created the web_pool backend with server web1, a public frontend on :443 with TLS, and routed www.example.com to it. The configuration validates. Open Review & apply to make it live."),
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		if step >= len(plan) {
			t.Errorf("model called more times (%d) than scripted", step+1)
			step = len(plan) - 1
		}
		resp := plan[step]
		step++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	if err := st.Bootstrap(context.Background(), store.BootstrapOptions{}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return st
}

// The agent must drive tool calls in order, mutate the store, validate, and
// return a coherent result — end to end, without a real model.
func TestAgentBuildsConfiguration(t *testing.T) {
	server := scriptedModel(t)
	defer server.Close()

	st := newTestStore(t)
	prev := &stubPreviewer{}
	env := &Env{Store: st, Preview: prev, Actor: "tester"}

	client := NewClient("test-key", "test-model")
	client.BaseURL = server.URL
	agent := NewAgent(client)

	result, err := agent.Run(context.Background(), env, nil,
		"Create an HTTPS site for www.example.com load balancing to 10.0.0.11:8080")
	if err != nil {
		t.Fatalf("agent run: %v", err)
	}

	if !result.Changed {
		t.Error("result.Changed = false, want true")
	}
	if !result.Valid {
		t.Error("result.Valid = false, want true")
	}
	if result.Reply == "" {
		t.Error("result.Reply is empty")
	}
	if prev.calls == 0 {
		t.Error("the agent never validated the configuration")
	}

	// The store must actually contain what the agent built.
	ctx := context.Background()
	backends, _ := st.ListBackends(ctx)
	if len(backends) != 1 || backends[0].Name != "web_pool" {
		t.Fatalf("backends = %+v, want one named web_pool", backends)
	}
	if !backends[0].HTTPChkEnabled {
		t.Error("backend health check was not enabled")
	}
	servers, _ := st.ListServers(ctx, backends[0].ID)
	if len(servers) != 1 || servers[0].Address != "10.0.0.11" || servers[0].Port != 8080 {
		t.Fatalf("servers = %+v, want web1 at 10.0.0.11:8080", servers)
	}

	frontends, _ := st.ListFrontends(ctx)
	if len(frontends) != 1 || frontends[0].Name != "public" {
		t.Fatalf("frontends = %+v, want one named public", frontends)
	}
	if !frontends[0].ForceHTTPS {
		t.Error("frontend force-HTTPS was not set")
	}
	binds, _ := st.ListBinds(ctx, frontends[0].ID)
	if len(binds) != 1 || binds[0].Port != 443 || !binds[0].SSL {
		t.Fatalf("binds = %+v, want one TLS bind on 443", binds)
	}
	domains, _ := st.ListDomains(ctx)
	if len(domains) != 1 || domains[0].Hostname != "www.example.com" {
		t.Fatalf("domains = %+v, want www.example.com", domains)
	}

	// A step trace must have been produced for the UI.
	if len(result.Steps) < 6 {
		t.Errorf("got %d steps, want at least 6", len(result.Steps))
	}
}

// A tool error must be reported back to the model rather than aborting the run.
func TestAgentToolErrorIsRecoverable(t *testing.T) {
	st := newTestStore(t)
	env := &Env{Store: st, Preview: &stubPreviewer{}, Actor: "tester"}
	reg := NewRegistry()

	// Adding a server to a non-existent backend must return an error string,
	// not panic or succeed.
	out, err := reg.Call(context.Background(), env, "add_server",
		`{"backend":"nope","name":"s1","address":"10.0.0.1","port":80}`)
	if err == nil {
		t.Fatalf("expected an error for a missing backend, got %q", out)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the missing backend: %v", err)
	}
}
