// SPDX-License-Identifier: Apache-2.0

package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/carloslfu/computer.md/daemon/persistence"
)

// Attachment is a single file the user uploaded with a message.
// Path is absolute on the daemon filesystem; the agent can reference it
// directly from bash / text_editor tool calls.
type Attachment struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	MIME     string `json:"mime"`
	Size     int64  `json:"size"`
	Original string `json:"original,omitempty"`
}

// TaskStatus represents the lifecycle state of a task.
type TaskStatus string

const (
	TaskQueued          TaskStatus = "queued"
	TaskRunning         TaskStatus = "running"
	TaskCompleted       TaskStatus = "completed"
	TaskFailed          TaskStatus = "failed"
	TaskWaitingForInput TaskStatus = "waiting_for_input"
	TaskCancelled       TaskStatus = "cancelled"
)

// Task represents a unit of work submitted by the user.
type Task struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	Instruction    string     `json:"instruction"`
	Status         TaskStatus `json:"status"`
	Result         *string    `json:"result,omitempty"`
	ErrorMessage   *string    `json:"error_message,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// Conversation groups related tasks and messages.
type Conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Message is a single message in a conversation.
type Message struct {
	ID             string       `json:"id"`
	ConversationID string       `json:"conversation_id"`
	Role           string       `json:"role"`
	Content        string       `json:"content"`
	Type           string       `json:"type"`
	ImageData      *string      `json:"image_data,omitempty"`
	Attachments    []Attachment `json:"attachments,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
}

// TaskStore handles persistence of tasks, conversations, and messages.
type TaskStore struct {
	db *persistence.DB
}

// NewTaskStore creates a TaskStore backed by the given database.
func NewTaskStore(db *persistence.DB) *TaskStore {
	return &TaskStore{db: db}
}

// CreateTask inserts a new task in queued status together with its user
// message in a single atomic transaction. Returns the task.
//
// The transaction matters: previously this method ran the two INSERTs
// as separate auto-committed Execs. The engine's NextQueued (a Query on
// the same single SQLite connection) could interleave between them,
// see the task row, dequeue it, call agentLoop → GetMessages, and find
// the conversation's last message was still the prior task's
// assistant turn. That apiMessages then ended on role=assistant, a bad
// request shape for manager APIs that expect a user turn next. Wrapping
// both writes in BEGIN…COMMIT makes the task INVISIBLE to NextQueued until
// the user message is also visible, closing the race at the source.
//
// Optional attachments are persisted on the generated user message so
// the agent can see them when it reads the conversation history.
func (s *TaskStore) CreateTask(conversationID, instruction string, attachments ...Attachment) (*Task, error) {
	id := uuid.New().String()
	now := time.Now().UTC()

	// Ensure conversation exists. Done outside the transaction because
	// ensureConversation is INSERT OR IGNORE — it's idempotent and the
	// tasks FK to conversations.id needs to be satisfied before we
	// start the inner transaction.
	if err := s.EnsureConversation(conversationID, instruction); err != nil {
		return nil, err
	}

	var attachmentsJSON *string
	if len(attachments) > 0 {
		data, err := json.Marshal(attachments)
		if err != nil {
			return nil, fmt.Errorf("encoding attachments: %w", err)
		}
		s := string(data)
		attachmentsJSON = &s
	}

	tx, err := s.db.Conn().Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	// Rollback is a no-op after a successful Commit; safe to defer here
	// even on the happy path. Failure path needs it or the connection
	// stays in an aborted transaction state.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`INSERT INTO tasks (id, conversation_id, instruction, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, conversationID, instruction, string(TaskQueued), now, now,
	); err != nil {
		return nil, fmt.Errorf("inserting task: %w", err)
	}

	msgID := uuid.New().String()
	if _, err := tx.Exec(
		`INSERT INTO messages (id, conversation_id, role, content, type, image_data, attachments, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		msgID, conversationID, "user", instruction, "text", nil, attachmentsJSON, now,
	); err != nil {
		return nil, fmt.Errorf("inserting user message: %w", err)
	}

	if _, err := tx.Exec(
		`UPDATE conversations SET updated_at = ? WHERE id = ?`, now, conversationID,
	); err != nil {
		return nil, fmt.Errorf("touching conversation: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &Task{
		ID:             id,
		ConversationID: conversationID,
		Instruction:    instruction,
		Status:         TaskQueued,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// GetTask retrieves a task by ID.
func (s *TaskStore) GetTask(id string) (*Task, error) {
	row := s.db.Conn().QueryRow(
		`SELECT id, conversation_id, instruction, status, result, error_message,
		        created_at, updated_at, started_at, completed_at
		 FROM tasks WHERE id = ?`, id,
	)
	return scanTask(row)
}

// UpdateStatus transitions a task to a new status.
//
// started_at is set only on the FIRST transition to TaskRunning (COALESCE so
// later transitions — e.g., resuming from TaskWaitingForInput — keep the
// original start timestamp). Otherwise the user-approval wait would reset the
// clock and the activity summary would misreport a long task as 4 seconds.
func (s *TaskStore) UpdateStatus(id string, status TaskStatus) error {
	now := time.Now().UTC()
	query := `UPDATE tasks SET status = ?, updated_at = ?`
	args := []interface{}{string(status), now}

	switch status {
	case TaskRunning:
		query += `, started_at = COALESCE(started_at, ?)`
		args = append(args, now)
	case TaskCompleted, TaskFailed, TaskCancelled:
		query += `, completed_at = ?`
		args = append(args, now)
	}

	query += ` WHERE id = ?`
	args = append(args, id)

	_, err := s.db.Conn().Exec(query, args...)
	return err
}

// SetResult sets the result text for a completed task.
func (s *TaskStore) SetResult(id, result string) error {
	_, err := s.db.Conn().Exec(
		`UPDATE tasks SET result = ?, status = ?, updated_at = ?, completed_at = ?
		 WHERE id = ?`,
		result, string(TaskCompleted), time.Now().UTC(), time.Now().UTC(), id,
	)
	return err
}

// SetError sets the error message for a failed task.
func (s *TaskStore) SetError(id, errMsg string) error {
	_, err := s.db.Conn().Exec(
		`UPDATE tasks SET error_message = ?, status = ?, updated_at = ?, completed_at = ?
		 WHERE id = ?`,
		errMsg, string(TaskFailed), time.Now().UTC(), time.Now().UTC(), id,
	)
	return err
}

// SetWaitingForInput marks a task as needing user input.
func (s *TaskStore) SetWaitingForInput(id, question string) error {
	_, err := s.db.Conn().Exec(
		`UPDATE tasks SET result = ?, status = ?, updated_at = ? WHERE id = ?`,
		question, string(TaskWaitingForInput), time.Now().UTC(), id,
	)
	return err
}

// ListByConversation returns all tasks for a conversation ordered by creation time.
func (s *TaskStore) ListByConversation(conversationID string) ([]*Task, error) {
	rows, err := s.db.Conn().Query(
		`SELECT id, conversation_id, instruction, status, result, error_message,
		        created_at, updated_at, started_at, completed_at
		 FROM tasks WHERE conversation_id = ? ORDER BY created_at ASC`, conversationID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		t, err := scanTaskRows(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// ListWaitingForInput returns every task currently in the
// waiting_for_input state. Used by the SSE stream handler to replay
// pending approval prompts to clients that connect (or reconnect) while
// an approval is outstanding.
func (s *TaskStore) ListWaitingForInput() ([]*Task, error) {
	rows, err := s.db.Conn().Query(
		`SELECT id, conversation_id, instruction, status, result, error_message,
		        created_at, updated_at, started_at, completed_at
		 FROM tasks WHERE status = ? ORDER BY created_at ASC`,
		string(TaskWaitingForInput),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		t, err := scanTaskRows(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// ListZombieTasks returns all tasks currently stuck in "running" or
// "waiting_for_input" state. Called on startup — the engine uses the result to
// write activity stubs for tasks that were interrupted by a daemon restart.
func (s *TaskStore) ListZombieTasks() ([]*Task, error) {
	rows, err := s.db.Conn().Query(
		`SELECT id, conversation_id, instruction, status, result, error_message,
		        created_at, updated_at, started_at, completed_at
		 FROM tasks WHERE status IN (?, ?) ORDER BY created_at ASC`,
		string(TaskRunning), string(TaskWaitingForInput),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		t, err := scanTaskRows(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// FailRunningTasks marks all tasks in "running" or "waiting_for_input" status
// as failed. Called on startup to clean up zombie tasks from a previous daemon run.
func (s *TaskStore) FailRunningTasks(reason string) (int, error) {
	now := time.Now().UTC()
	result, err := s.db.Conn().Exec(
		`UPDATE tasks SET status = ?, error_message = ?, updated_at = ?, completed_at = ?
		 WHERE status IN (?, ?)`,
		string(TaskFailed), reason, now, now,
		string(TaskRunning), string(TaskWaitingForInput),
	)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// NextQueued returns the oldest queued task, or nil if none.
func (s *TaskStore) NextQueued() (*Task, error) {
	row := s.db.Conn().QueryRow(
		`SELECT id, conversation_id, instruction, status, result, error_message,
		        created_at, updated_at, started_at, completed_at
		 FROM tasks WHERE status = ? ORDER BY created_at ASC LIMIT 1`,
		string(TaskQueued),
	)
	t, err := scanTask(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// EnsureConversation creates a conversation if it doesn't exist.
// Idempotent (INSERT OR IGNORE). Exported so callers that need to
// write messages into a conversation WITHOUT going through CreateTask
// (e.g. the budget gate, which holds a task but still records the
// exchange) can satisfy the messages→conversations foreign key first.
func (s *TaskStore) EnsureConversation(id, firstMessage string) error {
	now := time.Now().UTC()
	title := firstMessage
	if len(title) > 100 {
		title = title[:100]
	}
	_, err := s.db.Conn().Exec(
		`INSERT OR IGNORE INTO conversations (id, title, created_at, updated_at)
		 VALUES (?, ?, ?, ?)`,
		id, title, now, now,
	)
	return err
}

// GetConversation retrieves a conversation by ID.
func (s *TaskStore) GetConversation(id string) (*Conversation, error) {
	row := s.db.Conn().QueryRow(
		`SELECT id, title, created_at, updated_at FROM conversations WHERE id = ?`, id,
	)
	var c Conversation
	err := row.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListConversations returns all conversations ordered by most recent first.
func (s *TaskStore) ListConversations() ([]*Conversation, error) {
	rows, err := s.db.Conn().Query(
		`SELECT id, title, created_at, updated_at FROM conversations ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var convos []*Conversation
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		convos = append(convos, &c)
	}
	return convos, rows.Err()
}

// AddMessage stores a text message in a conversation.
func (s *TaskStore) AddMessage(conversationID, role, content string) error {
	return s.AddTypedMessage(conversationID, role, content, "text", nil)
}

// AddMessageWithAttachments stores a text message together with its file
// attachments. Attachments are encoded as JSON into the attachments column.
func (s *TaskStore) AddMessageWithAttachments(conversationID, role, content string, attachments []Attachment) error {
	_, err := s.addMessage(conversationID, role, content, "text", nil, attachments)
	return err
}

// AddScreenshotMessage stores a screenshot message in a conversation.
func (s *TaskStore) AddScreenshotMessage(conversationID, caption, imageData string) error {
	return s.AddTypedMessage(conversationID, "assistant", caption, "screenshot", &imageData)
}

// AddTypedMessage stores a message with a type and optional image data.
func (s *TaskStore) AddTypedMessage(conversationID, role, content, msgType string, imageData *string) error {
	_, err := s.addMessage(conversationID, role, content, msgType, imageData, nil)
	return err
}

// AddTypedMessageReturningID is AddTypedMessage but returns the generated id,
// for callers that need to update the message later (e.g. the credential
// request flow flips the persisted payload to "stored" on submission).
func (s *TaskStore) AddTypedMessageReturningID(conversationID, role, content, msgType string, imageData *string) (string, error) {
	return s.addMessage(conversationID, role, content, msgType, imageData, nil)
}

// UpdateMessageContent replaces the content column for an already-persisted
// message. The created_at timestamp is preserved so rehydration still
// orders the message correctly; only content changes.
func (s *TaskStore) UpdateMessageContent(id, content string) error {
	res, err := s.db.Conn().Exec(
		`UPDATE messages SET content = ? WHERE id = ?`, content, id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no message with id %s", id)
	}
	return nil
}

func (s *TaskStore) addMessage(conversationID, role, content, msgType string, imageData *string, attachments []Attachment) (string, error) {
	id := uuid.New().String()
	now := time.Now().UTC()

	var attachmentsJSON *string
	if len(attachments) > 0 {
		data, err := json.Marshal(attachments)
		if err != nil {
			return "", fmt.Errorf("encoding attachments: %w", err)
		}
		str := string(data)
		attachmentsJSON = &str
	}

	_, err := s.db.Conn().Exec(
		`INSERT INTO messages (id, conversation_id, role, content, type, image_data, attachments, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, conversationID, role, content, msgType, imageData, attachmentsJSON, now,
	)
	if err != nil {
		return "", err
	}
	_, _ = s.db.Conn().Exec(
		`UPDATE conversations SET updated_at = ? WHERE id = ?`, now, conversationID,
	)
	return id, nil
}

// GetMessages returns all messages for a conversation in order.
func (s *TaskStore) GetMessages(conversationID string) ([]*Message, error) {
	rows, err := s.db.Conn().Query(
		`SELECT id, conversation_id, role, content, type, image_data, attachments, created_at
		 FROM messages WHERE conversation_id = ? ORDER BY created_at ASC`,
		conversationID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		var m Message
		var attachmentsJSON sql.NullString
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Type, &m.ImageData, &attachmentsJSON, &m.CreatedAt); err != nil {
			return nil, err
		}
		if m.Type == "" {
			m.Type = "text"
		}
		if attachmentsJSON.Valid && attachmentsJSON.String != "" {
			_ = json.Unmarshal([]byte(attachmentsJSON.String), &m.Attachments)
		}
		msgs = append(msgs, &m)
	}
	return msgs, rows.Err()
}

// ListRecent returns the most recent tasks across all conversations.
func (s *TaskStore) ListRecent(limit int) ([]*Task, error) {
	rows, err := s.db.Conn().Query(
		`SELECT id, conversation_id, instruction, status, result, error_message,
		        created_at, updated_at, started_at, completed_at
		 FROM tasks ORDER BY created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		t, err := scanTaskRows(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func scanTask(row *sql.Row) (*Task, error) {
	var t Task
	var status string
	err := row.Scan(
		&t.ID, &t.ConversationID, &t.Instruction, &status,
		&t.Result, &t.ErrorMessage,
		&t.CreatedAt, &t.UpdatedAt, &t.StartedAt, &t.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	t.Status = TaskStatus(status)
	return &t, nil
}

func scanTaskRows(rows *sql.Rows) (*Task, error) {
	var t Task
	var status string
	err := rows.Scan(
		&t.ID, &t.ConversationID, &t.Instruction, &status,
		&t.Result, &t.ErrorMessage,
		&t.CreatedAt, &t.UpdatedAt, &t.StartedAt, &t.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	t.Status = TaskStatus(status)
	return &t, nil
}
