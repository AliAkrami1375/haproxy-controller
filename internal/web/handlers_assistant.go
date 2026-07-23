package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ebdaa/haproxy-controller/internal/ai"
	"github.com/ebdaa/haproxy-controller/internal/store"
)

// aiConfig holds the resolved assistant configuration for a request.
type aiConfig struct {
	Enabled bool
	APIKey  string
	Model   string
	BaseURL string
}

// loadAIConfig reads and decrypts the assistant settings.
func (s *Server) loadAIConfig(ctx context.Context) (aiConfig, error) {
	c := aiConfig{
		Enabled: s.store.GetSettingBool(ctx, store.SetAIEnabled, false),
		Model:   s.store.GetSetting(ctx, store.SetAIModel, ai.DefaultModel),
		BaseURL: s.store.GetSetting(ctx, store.SetAIBaseURL, ai.DefaultBaseURL),
	}
	stored := s.store.GetSetting(ctx, store.SetAIAPIKey, "")
	if stored != "" {
		key, err := ai.Decrypt(stored, s.cfg.SessionSecret)
		if err != nil {
			return c, err
		}
		c.APIKey = key
	}
	return c, nil
}

// aiReady reports whether the assistant is enabled and has a key.
func (s *Server) aiReady(ctx context.Context) bool {
	c, err := s.loadAIConfig(ctx)
	return err == nil && c.Enabled && c.APIKey != ""
}

// newAgentEnv builds the environment the agent's tools act on.
func (s *Server) newAgentEnv(r *http.Request) *ai.Env {
	actor := "assistant"
	if u := userFrom(r); u != nil {
		actor = u.Username
	}
	return &ai.Env{
		Store:   s.store,
		Preview: s.deployer,
		Actor:   actor,
	}
}

// ---------------------------------------------------------------- chat pages

func (s *Server) handleAssistant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userFrom(r)

	cfg, _ := s.loadAIConfig(ctx)
	conversations, _ := s.store.ListConversations(ctx, user.ID, 50)

	var (
		active   *store.Conversation
		messages []*store.ChatMessage
	)
	if raw := r.PathValue("id"); raw != "" {
		id, err := pathID(r, "id")
		if err == nil {
			active, _ = s.store.GetConversation(ctx, id, user.ID)
		}
	}
	// Land the operator in a usable conversation: the one requested, else the
	// most recent, else a fresh one. The composer needs an active thread.
	if active == nil && cfg.Enabled && cfg.APIKey != "" {
		if len(conversations) > 0 {
			active = conversations[0]
		} else if id, err := s.store.CreateConversation(ctx, &user.ID, "New conversation"); err == nil {
			active, _ = s.store.GetConversation(ctx, id, user.ID)
			conversations, _ = s.store.ListConversations(ctx, user.ID, 50)
		}
	}
	if active != nil {
		messages, _ = s.store.ListChatMessages(ctx, active.ID)
	}

	// Decode the stored step traces for rendering.
	type renderedMsg struct {
		*store.ChatMessage
		StepList []ai.Step
	}
	var rendered []renderedMsg
	for _, m := range messages {
		rm := renderedMsg{ChatMessage: m}
		if m.Steps != "" {
			_ = json.Unmarshal([]byte(m.Steps), &rm.StepList)
		}
		rendered = append(rendered, rm)
	}

	p := s.newPage(r, "Assistant", "assistant")
	p.Flash = s.takeFlash(w, r)
	p.Data["Ready"] = cfg.Enabled && cfg.APIKey != ""
	p.Data["Enabled"] = cfg.Enabled
	p.Data["HasKey"] = cfg.APIKey != ""
	p.Data["Model"] = cfg.Model
	p.Data["Conversations"] = conversations
	p.Data["Active"] = active
	p.Data["Messages"] = rendered
	s.render(w, r, "assistant.html", p)
}

func (s *Server) handleAssistantNew(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id, err := s.store.CreateConversation(r.Context(), &user.ID, "New conversation")
	if err != nil {
		s.fail(w, r, "/assistant", err)
		return
	}
	redirect(w, r, "/assistant/"+int64str(id))
}

func (s *Server) handleAssistantDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.notFound(w, r, "That conversation")
		return
	}
	user := userFrom(r)
	if err := s.store.DeleteConversation(r.Context(), id, user.ID); err != nil {
		s.fail(w, r, "/assistant", err)
		return
	}
	s.setFlash(w, r, "success", "Conversation deleted.", "")
	redirect(w, r, "/assistant")
}

// messageResponse is the JSON returned to the chat page after a turn.
type messageResponse struct {
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	Reply   string    `json:"reply,omitempty"`
	Steps   []ai.Step `json:"steps,omitempty"`
	Changed bool      `json:"changed"`
	Valid   bool      `json:"valid"`
	Tokens  int       `json:"tokens,omitempty"`
}

// handleAssistantMessage runs one agent turn for a conversation and returns
// the reply and step trace as JSON.
func (s *Server) handleAssistantMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userFrom(r)

	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, messageResponse{Error: "invalid conversation"})
		return
	}
	conv, err := s.store.GetConversation(ctx, id, user.ID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, messageResponse{Error: "conversation not found"})
		return
	}

	prompt := strings.TrimSpace(r.PostFormValue("message"))
	if prompt == "" {
		writeJSON(w, http.StatusBadRequest, messageResponse{Error: "empty message"})
		return
	}
	if len(prompt) > 8000 {
		writeJSON(w, http.StatusBadRequest, messageResponse{Error: "message is too long"})
		return
	}

	cfg, err := s.loadAIConfig(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messageResponse{Error: err.Error()})
		return
	}
	if !cfg.Enabled || cfg.APIKey == "" {
		writeJSON(w, http.StatusBadRequest, messageResponse{
			Error: "The assistant is not configured. An administrator must set an OpenRouter API key under Settings.",
		})
		return
	}

	// Rebuild the model transcript from the stored history so the conversation
	// continues with full context.
	history, err := s.buildTranscript(ctx, conv.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messageResponse{Error: err.Error()})
		return
	}

	client := ai.NewClient(cfg.APIKey, cfg.Model)
	client.BaseURL = cfg.BaseURL
	agent := ai.NewAgent(client)
	env := s.newAgentEnv(r)

	// Give a long turn room to run several tool calls and repairs.
	runCtx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	result, runErr := agent.Run(runCtx, env, history, prompt)
	if runErr != nil {
		s.log.Warn("assistant run failed", "user", user.Username, "error", runErr)
		writeJSON(w, http.StatusOK, messageResponse{Error: runErr.Error()})
		return
	}

	// Persist the user turn and the assistant turn (with its step trace and the
	// raw model messages needed to continue).
	transcript, _ := json.Marshal(result.Messages)
	steps, _ := json.Marshal(result.Steps)

	if _, err := s.store.AddChatMessage(ctx, &store.ChatMessage{
		ConversationID: conv.ID, Role: "user", Content: prompt,
	}); err != nil {
		s.log.Error("persist user message", "error", err)
	}
	if _, err := s.store.AddChatMessage(ctx, &store.ChatMessage{
		ConversationID: conv.ID, Role: "assistant", Content: result.Reply,
		Steps: string(steps), Transcript: string(transcript),
		Changed: result.Changed, Valid: result.Valid, Tokens: result.TokensUsed,
	}); err != nil {
		s.log.Error("persist assistant message", "error", err)
	}

	// Name the thread from its first message.
	if conv.Title == "New conversation" {
		_ = s.store.SetConversationTitle(ctx, conv.ID, firstSentence(prompt))
	}

	s.audit(r, "assistant.turn", "assistant", int64str(conv.ID),
		firstSentence(prompt))

	writeJSON(w, http.StatusOK, messageResponse{
		OK: true, Reply: result.Reply, Steps: result.Steps,
		Changed: result.Changed, Valid: result.Valid, Tokens: result.TokensUsed,
	})
}

// buildTranscript reconstructs the model message history from stored assistant
// transcripts, so a continued conversation keeps the tool-call context. Only
// the compact user/assistant text is kept for older turns to bound the size.
func (s *Server) buildTranscript(ctx context.Context, conversationID int64) ([]ai.Message, error) {
	stored, err := s.store.ListChatMessages(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	var out []ai.Message
	for _, m := range stored {
		if m.Role == "user" {
			out = append(out, ai.Message{Role: ai.RoleUser, Content: m.Content})
			continue
		}
		// For assistant turns, keep only the final text in the continued
		// context. Replaying tool calls and their results would balloon the
		// prompt; the resulting state is already in the database, which the
		// agent reads fresh via get_overview.
		out = append(out, ai.Message{Role: ai.RoleAssistant, Content: m.Content})
	}
	return out, nil
}

func firstSentence(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	for i, r := range s {
		if r == '.' || r == '?' || r == '!' {
			return strings.TrimSpace(s[:i+1])
		}
		if i >= 60 {
			return strings.TrimSpace(s[:i]) + "…"
		}
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ----------------------------------------------------- admin: AI settings

func (s *Server) handleAISettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, _ := s.loadAIConfig(ctx)

	p := s.newPage(r, "Assistant settings", "settings")
	p.Flash = s.takeFlash(w, r)
	p.Data["Enabled"] = cfg.Enabled
	p.Data["Model"] = cfg.Model
	p.Data["BaseURL"] = cfg.BaseURL
	p.Data["HasKey"] = cfg.APIKey != ""
	p.Data["KeyHint"] = ai.MaskKey(cfg.APIKey)
	p.Data["DefaultModel"] = ai.DefaultModel
	s.render(w, r, "ai_settings.html", p)
}

func (s *Server) handleAISettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	_ = s.store.SetSettingBool(ctx, store.SetAIEnabled, formBool(r, "enabled"))

	if model := formStr(r, "model"); model != "" {
		_ = s.store.SetSetting(ctx, store.SetAIModel, model)
	}
	if base := formStr(r, "base_url"); base != "" {
		_ = s.store.SetSetting(ctx, store.SetAIBaseURL, base)
	}

	// Only replace the key when a new one is supplied, so saving other fields
	// does not wipe it. A literal "clear" removes it.
	newKey := strings.TrimSpace(r.PostFormValue("api_key"))
	switch {
	case newKey == "clear":
		_ = s.store.SetSetting(ctx, store.SetAIAPIKey, "")
	case newKey != "":
		enc, err := ai.Encrypt(newKey, s.cfg.SessionSecret)
		if err != nil {
			s.fail(w, r, "/settings/ai", err)
			return
		}
		_ = s.store.SetSetting(ctx, store.SetAIAPIKey, enc)
	}

	s.audit(r, "ai.settings_updated", "settings", "", "")
	s.setFlash(w, r, "success", "Assistant settings saved.", "")
	redirect(w, r, "/settings/ai")
}

// handleAIModels returns the free, tool-capable models as JSON for the picker.
func (s *Server) handleAIModels(w http.ResponseWriter, r *http.Request) {
	cfg, _ := s.loadAIConfig(r.Context())
	client := ai.NewClient(cfg.APIKey, cfg.Model)
	client.BaseURL = cfg.BaseURL

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	models, err := client.ListFreeToolModels(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		return
	}
	type m struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Context int    `json:"context"`
	}
	out := make([]m, 0, len(models))
	for _, mi := range models {
		out = append(out, m{ID: mi.ID, Name: mi.Name, Context: mi.ContextLength})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

// handleAITest sends a trivial prompt to confirm the key and model work.
func (s *Server) handleAITest(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadAIConfig(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if cfg.APIKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "no API key is set"})
		return
	}
	client := ai.NewClient(cfg.APIKey, cfg.Model)
	client.BaseURL = cfg.BaseURL

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	comp, err := client.Complete(ctx, []ai.Message{
		{Role: ai.RoleUser, Content: "Reply with the single word: ready"},
	}, nil, 0)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "model": cfg.Model,
		"reply": strings.TrimSpace(comp.Message.Content),
	})
}
