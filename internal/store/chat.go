package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Conversation is one assistant chat thread owned by a user.
type Conversation struct {
	ID        int64
	UserID    *int64
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ChatMessage is one turn in a conversation. Assistant turns carry the agent's
// step trace and the raw model transcript used to continue the conversation.
type ChatMessage struct {
	ID             int64
	ConversationID int64
	Role           string // user | assistant
	Content        string
	Steps          string // JSON array of agent steps
	Transcript     string // JSON array of model messages
	Changed        bool
	Valid          bool
	Tokens         int
	CreatedAt      time.Time
}

// CreateConversation starts a new thread and returns its id.
func (s *Store) CreateConversation(ctx context.Context, userID *int64, title string) (int64, error) {
	if title == "" {
		title = "New conversation"
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	res, err := s.db.ExecContext(c,
		"INSERT INTO ai_conversations (user_id, title) VALUES (?, ?)", nullInt64(userID), truncate(title, 120))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListConversations returns a user's threads, most recently updated first.
func (s *Store) ListConversations(ctx context.Context, userID int64, limit int) ([]*Conversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, `
		SELECT id, user_id, title, created_at, updated_at
		FROM ai_conversations WHERE user_id = ?
		ORDER BY updated_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Conversation
	for rows.Next() {
		conv, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, conv)
	}
	return out, rows.Err()
}

// GetConversation loads one thread, scoped to its owner.
func (s *Store) GetConversation(ctx context.Context, id, userID int64) (*Conversation, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanConversation(s.db.QueryRowContext(c, `
		SELECT id, user_id, title, created_at, updated_at
		FROM ai_conversations WHERE id = ? AND user_id = ?`, id, userID))
}

func scanConversation(row interface{ Scan(...any) error }) (*Conversation, error) {
	var (
		conv             Conversation
		userID           sql.NullInt64
		created, updated sql.NullString
	)
	err := row.Scan(&conv.ID, &userID, &conv.Title, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if userID.Valid {
		id := userID.Int64
		conv.UserID = &id
	}
	conv.CreatedAt = parseTime(created)
	conv.UpdatedAt = parseTime(updated)
	return &conv, nil
}

// SetConversationTitle renames a thread, e.g. from the first user message.
func (s *Store) SetConversationTitle(ctx context.Context, id int64, title string) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c,
		"UPDATE ai_conversations SET title = ? WHERE id = ?", truncate(title, 120), id)
	return err
}

// DeleteConversation removes a thread and its messages.
func (s *Store) DeleteConversation(ctx context.Context, id, userID int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c,
		"DELETE FROM ai_conversations WHERE id = ? AND user_id = ?", id, userID)
	return err
}

// AddChatMessage appends a message and bumps the thread's updated_at.
func (s *Store) AddChatMessage(ctx context.Context, m *ChatMessage) (int64, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(c, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(c, `
		INSERT INTO ai_messages (conversation_id, role, content, steps, transcript, changed, valid, tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ConversationID, m.Role, m.Content, m.Steps, m.Transcript,
		boolToInt(m.Changed), boolToInt(m.Valid), m.Tokens)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(c,
		"UPDATE ai_conversations SET updated_at = datetime('now') WHERE id = ?", m.ConversationID); err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// ListChatMessages returns a conversation's messages in order.
func (s *Store) ListChatMessages(ctx context.Context, conversationID int64) ([]*ChatMessage, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, `
		SELECT id, conversation_id, role, content, steps, transcript, changed, valid, tokens, created_at
		FROM ai_messages WHERE conversation_id = ? ORDER BY id`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*ChatMessage
	for rows.Next() {
		var (
			m              ChatMessage
			changed, valid int
			created        sql.NullString
		)
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Steps,
			&m.Transcript, &changed, &valid, &m.Tokens, &created); err != nil {
			return nil, err
		}
		m.Changed = changed != 0
		m.Valid = valid != 0
		m.CreatedAt = parseTime(created)
		out = append(out, &m)
	}
	return out, rows.Err()
}
