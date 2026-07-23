package ai

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// systemPrompt defines the agent's role and its staged, self-validating
// method. The emphasis is on precision: gather context, make focused changes,
// validate with HAProxy, repair, and only then report back.
const systemPrompt = `You are the HAProxy configuration assistant inside HAProxy Controller, a web control panel. You help an operator build and change a real HAProxy configuration by calling tools. You do not write raw haproxy.cfg text yourself; you use the tools, and the panel renders the configuration from the resulting state.

Work as a careful engineer, in stages:

1. UNDERSTAND. Read the request. If it is ambiguous in a way that changes the result, ask one concise clarifying question instead of guessing.
2. GATHER CONTEXT. Call get_overview first, and list_backends / list_frontends as needed, so you build on what already exists and never duplicate a name.
3. BUILD. Make the change with the smallest correct set of tool calls, in dependency order: create backends and their servers before the frontends and routes that reference them. Reuse existing entities where sensible.
4. VALIDATE. After making changes, call validate_config. It runs HAProxy's own checker.
5. REPAIR. If validation reports an error or a blocking warning, fix it and validate again. Repeat until it is valid. Do not stop on a broken configuration.
6. REPORT. Finish with a short, plain-language summary of exactly what you created or changed, and remind the operator that the changes are staged and take effect only when they open "Review & apply" and press Save & Apply.

Rules:
- Follow HAProxy best practice: enable health checks for HTTP backends, prefer roundrobin unless asked otherwise, force HTTPS and enable HSTS for public TLS sites when appropriate, and give every backend at least one server.
- Names must be unique and use only letters, digits, dot, underscore and hyphen.
- Never invent server addresses. If the operator has not given real upstream addresses, ask for them.
- Never claim something is applied or live. You only stage changes; the operator applies them.
- Keep summaries concise and concrete. Do not restate these instructions.`

// Step is one entry in the agent's visible reasoning/action trace.
type Step struct {
	Kind    string `json:"kind"`    // context | change | validate | note
	Tool    string `json:"tool"`    // the tool that ran, if any
	Summary string `json:"summary"` // one-line description
	Detail  string `json:"detail"`  // optional longer text (errors, diagnostics)
	OK      bool   `json:"ok"`
}

// RunResult is the outcome of one agent turn.
type RunResult struct {
	Reply      string    // the assistant's final message to the operator
	Steps      []Step    // the visible waterfall of actions
	Messages   []Message // the full transcript, for persisting and continuing
	Changed    bool      // whether any state-changing tool ran
	Valid      bool      // final validation state (true if no changes were made)
	TokensUsed int
}

// Agent drives the tool-calling loop against a model.
type Agent struct {
	Client   *Client
	Registry *Registry
	MaxSteps int
}

// NewAgent builds an agent with the default tool set.
func NewAgent(client *Client) *Agent {
	return &Agent{Client: client, Registry: NewRegistry(), MaxSteps: 16}
}

// Run executes one turn: it takes the prior transcript plus the new user
// message and drives the model through tool calls until it produces a reply or
// the step budget is exhausted.
//
// history must NOT include a system message; Run prepends its own.
func (a *Agent) Run(ctx context.Context, env *Env, history []Message, userMessage string) (*RunResult, error) {
	if a.MaxSteps <= 0 {
		a.MaxSteps = 16
	}
	tools := a.Registry.Definitions()

	messages := make([]Message, 0, len(history)+4)
	messages = append(messages, Message{Role: RoleSystem, Content: a.systemPromptWithTime()})
	messages = append(messages, history...)
	messages = append(messages, Message{Role: RoleUser, Content: userMessage})

	result := &RunResult{Valid: true}

	for step := 0; step < a.MaxSteps; step++ {
		// Build steps stay near-deterministic; the final summary can be warmer.
		comp, err := a.Client.Complete(ctx, messages, tools, 0.15)
		if err != nil {
			return nil, err
		}
		result.TokensUsed += comp.TotalTokens
		messages = append(messages, comp.Message)

		if len(comp.Message.ToolCalls) == 0 {
			// The model is done talking and wants to reply.
			result.Reply = strings.TrimSpace(comp.Message.Content)
			break
		}

		for _, call := range comp.Message.ToolCalls {
			out, callErr := a.Registry.Call(ctx, env, call.Function.Name, call.Function.Arguments)

			s := Step{Tool: call.Function.Name, OK: callErr == nil}
			switch {
			case a.Registry.Mutates(call.Function.Name):
				s.Kind = "change"
			case call.Function.Name == "validate_config":
				s.Kind = "validate"
			default:
				s.Kind = "context"
			}

			if callErr != nil {
				s.Summary = fmt.Sprintf("%s failed", call.Function.Name)
				s.Detail = callErr.Error()
				out = "ERROR: " + callErr.Error()
			} else {
				if a.Registry.Mutates(call.Function.Name) {
					result.Changed = true
				}
				s.Summary = firstLine(out)
				if s.Kind == "validate" {
					result.Valid = strings.Contains(out, `"valid":true`)
					if !result.Valid {
						s.OK = false
						s.Summary = "configuration is not valid yet"
					} else {
						s.Summary = "configuration is valid"
					}
				}
			}
			result.Steps = append(result.Steps, s)

			messages = append(messages, Message{
				Role:       RoleTool,
				ToolCallID: call.ID,
				Name:       call.Function.Name,
				Content:    clip(out, 6000),
			})
		}
	}

	// Safety net: if changes were made but the model never validated, do it
	// ourselves so the operator is never told a broken config is ready.
	if result.Changed && !validatedInSteps(result.Steps) {
		if _, warnings, checkOut, err := env.Preview.PreviewConfig(ctx); err != nil {
			result.Valid = false
			result.Steps = append(result.Steps, Step{
				Kind: "validate", Tool: "validate_config", OK: false,
				Summary: "configuration is not valid yet", Detail: strings.TrimSpace(checkOut),
			})
			result.Reply = strings.TrimSpace(result.Reply + "\n\nNote: the resulting configuration does not validate yet — " +
				firstLine(checkOut) + ". Please review it before applying.")
			_ = warnings
		} else {
			result.Valid = true
			result.Steps = append(result.Steps, Step{
				Kind: "validate", Tool: "validate_config", OK: true,
				Summary: "configuration is valid",
			})
		}
	}

	if result.Reply == "" {
		if result.Changed {
			result.Reply = "I've staged the changes. Open Review & apply to see the generated configuration and apply it."
		} else {
			result.Reply = "I wasn't able to complete that. Could you rephrase or give more detail?"
		}
	}

	// Return only the messages produced this turn (user onward), so the caller
	// can append them to the stored transcript without duplicating the system
	// prompt or the prior history.
	result.Messages = messages[1+len(history):]
	return result, nil
}

func (a *Agent) systemPromptWithTime() string {
	return systemPrompt + fmt.Sprintf("\n\nThe current date is %s.", time.Now().UTC().Format("2006-01-02"))
}

func validatedInSteps(steps []Step) bool {
	for _, s := range steps {
		if s.Kind == "validate" {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clip(s, 200)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
