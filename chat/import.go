package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Conversation import, for moving history from another chat system into this
// one: a host's own conversation tables (ImportStore.ImportConversation, from
// server code) or threads a browser kept locally (POST /conversations/actions/import,
// text only).
//
// An imported conversation is ordinary afterwards: it lists, opens and
// continues like any other. Its history keeps roles, timestamps, tool calls and
// results, and the outcomes of past proposals as notices; nothing in it is
// actionable, because the proposals themselves are not imported.

// ImportedConversation is one conversation to import.
type ImportedConversation struct {
	// SourceID identifies the conversation in the system it comes from. An
	// import with a SourceID the owner already imported returns that
	// conversation instead of a copy, so an import can be retried.
	SourceID  string
	Title     string
	Target    string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Messages in order. Each has a Role, Parts and, optionally, CreatedAt;
	// IDs, ConversationID and TurnID are assigned by the store. Allowed parts:
	// text on user and assistant messages, tool_call on assistant messages,
	// tool_result on tool messages, attachment on user messages, and
	// proposal_outcome on notices. An attachment part carries
	// the file's Name and MIME only: the bytes aren't imported, and the model
	// is told the file was omitted.
	Messages []Message
}

// ImportStore is the optional persistence of imports. Both bundled stores
// implement it.
type ImportStore interface {
	// ImportConversation stores the conversation and its messages in one
	// transaction. created is false, and nothing is written, when the owner
	// already imported SourceID; the earlier conversation is returned. It
	// validates with ValidateImport first.
	ImportConversation(ctx context.Context, owner OwnerKey, c ImportedConversation) (conv Conversation, created bool, err error)
}

// Import limits.
const (
	MaxImportMessages     = 2000
	maxImportSourceIDLen  = 200
	maxImportTitleRunes   = 200
	maxImportedPartBytes  = 1 << 20
	maxImportedTotalBytes = 32 << 20
)

// ErrInvalidImport wraps every ValidateImport refusal.
var ErrInvalidImport = errors.New("chat: invalid import")

// ValidateImport checks an import before anything is stored, and fills the
// timestamps an import left out: CreatedAt defaults to the first message's
// time (or now), UpdatedAt to the last message's, and each message is made at
// least a millisecond later than the one before it (stores order messages by
// time, at millisecond precision), so the transcript keeps its order.
func ValidateImport(c *ImportedConversation, now time.Time) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalidImport}, args...)...)
	}
	// Normalize a copy: the caller's messages and parts are its own, and may
	// be shared with another import running at the same time.
	c.Messages = cloneMessages(c.Messages)
	c.SourceID = strings.TrimSpace(c.SourceID)
	if c.SourceID == "" || len(c.SourceID) > maxImportSourceIDLen {
		return fail("sourceId must be 1 to %d characters", maxImportSourceIDLen)
	}
	if r := []rune(strings.TrimSpace(c.Title)); len(r) > maxImportTitleRunes {
		c.Title = string(r[:maxImportTitleRunes])
	}
	c.Title = strings.TrimSpace(c.Title)
	if len(c.Messages) == 0 || len(c.Messages) > MaxImportMessages {
		return fail("an import needs 1 to %d messages", MaxImportMessages)
	}
	total := 0
	var last time.Time
	for i := range c.Messages {
		m := &c.Messages[i]
		m.ID, m.ConversationID, m.TurnID = "", "", ""
		switch m.Role {
		case RoleUser, RoleAssistant, RoleTool, RoleNotice:
		default:
			return fail("message %d has role %q", i, m.Role)
		}
		if len(m.Parts) == 0 {
			return fail("message %d has no parts", i)
		}
		for k, p := range m.Parts {
			allowed := false
			switch p.Type {
			case PartText:
				allowed = m.Role != RoleNotice
			case PartToolCall:
				allowed = m.Role == RoleAssistant && p.CallID != "" && p.Tool != ""
			case PartToolResult:
				allowed = m.Role == RoleTool && p.CallID != ""
			case PartAttachment:
				allowed = m.Role == RoleUser && p.Name != ""
				// The bytes aren't imported: the reference names the file only.
				m.Parts[k].UploadID = ""
			case PartProposalOutcome:
				allowed = m.Role == RoleNotice && p.Text != ""
			}
			if !allowed {
				return fail("message %d part %d (%s) is not allowed on a %s message", i, k, p.Type, m.Role)
			}
			for _, raw := range []json.RawMessage{p.Arguments, p.Result} {
				if len(raw) > 0 && !json.Valid(raw) {
					return fail("message %d part %d carries invalid JSON", i, k)
				}
			}
			size := len(p.Text) + len(p.Arguments) + len(p.Result)
			if size > maxImportedPartBytes {
				return fail("message %d part %d is over %d bytes", i, k, maxImportedPartBytes)
			}
			total += size
		}
		at := m.CreatedAt.UTC().Truncate(time.Millisecond)
		if at.IsZero() && last.IsZero() {
			at = now.UTC().Truncate(time.Millisecond)
		}
		if !last.IsZero() && !at.After(last) {
			// Keep the order the import gave, even if its clocks disagree or
			// it gave no time at all.
			at = last.Add(time.Millisecond)
		}
		m.CreatedAt = at
		last = at
	}
	if total > maxImportedTotalBytes {
		return fail("the import is over %d bytes", maxImportedTotalBytes)
	}
	if c.CreatedAt.IsZero() || c.CreatedAt.After(c.Messages[0].CreatedAt) {
		c.CreatedAt = c.Messages[0].CreatedAt
	}
	if c.UpdatedAt.Before(last) {
		c.UpdatedAt = last
	}
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return nil
}

// ImportConversation imports through store, which must implement ImportStore.
func ImportConversation(ctx context.Context, store Store, owner OwnerKey, c ImportedConversation) (Conversation, bool, error) {
	importer, ok := store.(ImportStore)
	if !ok {
		return Conversation{}, false, errors.New("chat: the store does not support imports")
	}
	return importer.ImportConversation(ctx, owner, c)
}

// ImportTitle is the title of an untitled import: the first user message's
// first line, cut like a generated title.
func ImportTitle(c ImportedConversation) string {
	for _, m := range c.Messages {
		if m.Role != RoleUser {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == PartText && strings.TrimSpace(p.Text) != "" {
				return CleanTitle(p.Text)
			}
		}
	}
	return ""
}
