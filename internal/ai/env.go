package ai

import (
	"context"
	"strings"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

// Previewer renders the current configuration and validates it with HAProxy,
// without applying anything. The agent uses it to check its own work.
type Previewer interface {
	// PreviewConfig returns the rendered configuration, any renderer warnings,
	// HAProxy's validation output, and an error when the configuration is
	// invalid or could not be produced.
	PreviewConfig(ctx context.Context) (config string, warnings []string, checkOutput string, err error)
}

// Env is the environment the agent's tools act on: the data store, a way to
// validate, and the acting user for audit records.
type Env struct {
	Store     *store.Store
	Preview   Previewer
	Actor     string
	requestIP string
}

// backendByName resolves a backend by its name, case-insensitively.
func (e *Env) backendByName(ctx context.Context, name string) (*store.Backend, error) {
	list, err := e.Store.ListBackends(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range list {
		if strings.EqualFold(b.Name, name) {
			return b, nil
		}
	}
	return nil, nil
}

// frontendByName resolves a frontend by its name, case-insensitively.
func (e *Env) frontendByName(ctx context.Context, name string) (*store.Frontend, error) {
	list, err := e.Store.ListFrontends(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range list {
		if strings.EqualFold(f.Name, name) {
			return f, nil
		}
	}
	return nil, nil
}

// audit records an agent-performed change under the acting user's name.
func (e *Env) audit(ctx context.Context, action, entity, entityID, detail string) {
	_ = e.Store.Audit(ctx, store.AuditEntry{
		Username: e.Actor,
		Action:   action,
		Entity:   entity,
		EntityID: entityID,
		Detail:   "via assistant: " + detail,
		IP:       e.requestIP,
	})
}
