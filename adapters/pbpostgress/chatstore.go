package pbpostgress

// Fork ChatStore: the durable chat.Store, chat.ProposalStore and
// chat.UploadStore for the pb-postgress fork.
//
// It is the stock adapter's chat store (adapters/pocketbase/chatstore.go) ported
// to a fork app whose data lives in PostgreSQL instead of SQLite (the fork is
// PostgreSQL-only since v0.41.7). The public shape is deliberately identical,
// but four things had to change:
//
//  1. Plain uniqueness. The store was written when the fork also ran on MySQL,
//     which has no partial indexes ("UNIQUE ... WHERE x != ''"), so every
//     invariant uses a plain UNIQUE index over a column whose value is unique
//     by construction. Existing installations have these indexes, so they
//     stay:
//       - turns.activeSlot holds the conversation id while the turn is
//         non-terminal, and "done:<turn id>" once it is terminal, so
//         UNIQUE(activeSlot) means "one active turn per conversation";
//       - turns.idempotencySlot holds a fixed-length digest of
//         (ownerKey, conversation, client key) when the client sent an
//         Idempotency-Key, and "none:<turn id>" otherwise, so
//         UNIQUE(idempotencySlot) is exactly "one turn per
//         (owner, conversation, key)" and never confuses two keyless turns.
//         A digest keeps the index at 68 characters instead of a
//         three-column index over 255-character columns;
//       - events keeps UNIQUE(turn, seq).
//
//  2. A shared database with several app instances. Chat collections are
//     System collections, so the fork's per-instance collection scope
//     (core.BaseApp.CollectionVisible) can never hide them, while nil API
//     rules still keep them superuser-only on the records REST API. They are
//     never added to a manifest. Recovery is scoped: the store is built with
//     the chat namespace and mount and InterruptRunning only touches turns in
//     that scope, so restarting one app cannot interrupt another app's turns.
//
//  3. PostgreSQL transactions really run concurrently across app
//     instances (unlike SQLite, where every write transaction shares one
//     connection). State compare-and-swap, per-turn sequence assignment and
//     turn creation therefore take a row lock with SELECT ... FOR UPDATE and
//     re-read the row before deciding: the unique indexes remain the last
//     line of defence, but a lost update cannot slip between the read and
//     the write. READ COMMITTED means a statement after the lock sees the
//     winner's committed state, so no snapshot juggling is needed.
//
//  4. JSON payloads are stored as TEXT, not as the fork's json/jsonb column.
//     PostgreSQL JSONB normalizes the value it stores (key order, whitespace,
//     duplicate keys), but chat.Store must return the caller's bytes exactly
//     as written (the shared store contract checks this), so the columns hold
//     the raw JSON text and the store unmarshals on read. PostgreSQL TEXT has
//     no limit; a payload may be as long as the field allows
//     (chatMaxJSONBytes, 4 MiB, as on stock PocketBase), and the store
//     refuses a longer one with a clear error before the database sees it.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/dbutils"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/compdani/pocketbase-mcp/chat"
)

// DefaultChatCollectionPrefix is the collection-name prefix used when
// ChatStoreOptions.Prefix is empty.
const DefaultChatCollectionPrefix = "_mcpChat"

// Chat store field names. They document the provisioned schema; the object
// schema itself is not part of the chat contract.
const (
	chatFieldOwnerKey        = "ownerKey"
	chatFieldNamespace       = "namespace"
	chatFieldMount           = "mount"
	chatFieldAuthCollection  = "authCollectionId"
	chatFieldUserID          = "userId"
	chatFieldTenant          = "tenant"
	chatFieldTitle           = "title"
	chatFieldTarget          = "target"
	chatFieldCreatedAt       = "createdAt"
	chatFieldUpdatedAt       = "updatedAt"
	chatFieldConversation    = "conversation"
	chatFieldTurn            = "turn"
	chatFieldRole            = "role"
	chatFieldParts           = "parts"
	chatFieldState           = "state"
	chatFieldIdempotencyKey  = "idempotencyKey"
	chatFieldIdempotencySlot = "idempotencySlot"
	chatFieldActiveSlot      = "activeSlot"
	chatFieldProvider        = "provider"
	chatFieldModel           = "model"
	chatFieldEffort          = "effort"
	chatFieldLastSeq         = "lastSeq"
	chatFieldDroppedBefore   = "droppedBefore"
	chatFieldError           = "error"
	chatFieldSeq             = "seq"
	chatFieldType            = "type"
	chatFieldVersion         = "version"
	chatFieldData            = "data"
	chatFieldAt              = "at"
	// chatFieldContextSummary holds the conversation's summary (a
	// chat.Summary as JSON text), or nothing.
	chatFieldContextSummary = "contextSummary"
	// chatFieldImportSource is the SourceID of an imported conversation.
	chatFieldImportSource = "importSource"

	// Proposal fields. They keep the chat.Proposal names, except that the
	// conversation and turn bindings use their own column names so a proposal
	// row never collides with the turn columns of the other collections.
	chatFieldProposalConversation = "conversationId"
	chatFieldProposalTurn         = "turnId"
	chatFieldCallID               = "callId"
	chatFieldTool                 = "tool"
	chatFieldToolFingerprint      = "toolFingerprint"
	chatFieldManifestRevision     = "manifestRevision"
	chatFieldLocation             = "location"
	chatFieldArguments            = "arguments"
	chatFieldArgumentsHash        = "argumentsHash"
	chatFieldSummary              = "summary"
	chatFieldRevision             = "revision"
	chatFieldExpiresAt            = "expiresAt"
	chatFieldDecidedAt            = "decidedAt"
	chatFieldResult               = "result"
	chatFieldFailure              = "failure"
	chatFieldEditorRevision       = "editorRevision"
	chatFieldWarnings             = "warnings"
	chatFieldHunks                = "hunks"

	// Upload fields. The conversation binding uses its own column name so an
	// upload row never collides with the turn columns of the other
	// collections, and the bytes live in a protected file field so the app
	// stores them in its configured filesystem.
	chatFieldUploadConversation = "conversationId"
	chatFieldUploadName         = "name"
	chatFieldUploadMIME         = "mime"
	chatFieldUploadKind         = "kind"
	chatFieldUploadSize         = "size"
	chatFieldUploadSHA256       = "sha256"
	chatFieldUploadData         = "data"

	// chatMaxTextLen bounds every short text field, so an over-long value is
	// a validation error. Existing collections carry this bound, so it stays.
	chatMaxTextLen = 255
	// chatSlotMaxLen bounds the two derived uniqueness columns. Their real
	// content is at most 68 characters ("key:" + 64 hex chars).
	chatSlotMaxLen = 200
	// chatJSONFieldMaxChars is the field maximum of every JSON payload
	// column (an unbounded TEXT column on PostgreSQL).
	chatJSONFieldMaxChars = 4 << 20
	// chatMaxJSONBytes is the payload limit: the field's own maximum (in
	// characters, so at least as many bytes for ASCII), checked in Go so an
	// over-long payload fails with a clear error.
	chatMaxJSONBytes = chatJSONFieldMaxChars
	// chatMaxIdempotencyKeyBytes bounds one client Idempotency-Key. Keys are
	// stored verbatim (they are returned to the caller) but never indexed:
	// the index covers the digest column instead.
	chatMaxIdempotencyKeyBytes = 4 << 10
	// chatIdempotencyKeyFieldMax is the storage bound for a client key (the
	// key is not part of any index); chatMaxIdempotencyKeyBytes is the real
	// limit.
	chatIdempotencyKeyFieldMax = 1 << 20

	chatDefaultPageLimit = 50
	chatMaxPageLimit     = 200
	chatCursorSeparator  = "|"
	chatMaxTxAttempts    = 5
	// chatCollectionNameMaxLen leaves room for the longest generated index
	// name ("idx_" + name + "_owner_conversation") within PostgreSQL's
	// 63-byte identifier limit.
	chatCollectionNameMaxLen = 40

	chatInterruptedMessage = "turn interrupted by server restart"
)

// ChatStoreOptions configures NewChatStore.
type ChatStoreOptions struct {
	// Prefix is prepended to every provisioned collection name. Empty means
	// DefaultChatCollectionPrefix. It must be a safe identifier
	// ([A-Za-z_][A-Za-z0-9_]*) so collection names never need quoting tricks.
	Prefix string
	// MaxEventsPerTurn bounds the retained replay log of one turn. Older
	// events are dropped first, which makes an old cursor report
	// chat.ErrResyncRequired. Zero means chat.DefaultMaxEventsPerTurn.
	MaxEventsPerTurn int
	// Namespace and Mount scope restart recovery. Set both (EnableChat does)
	// so InterruptRunning only ends turns of this app and mount; leave both
	// empty for an unscoped store that owns the whole database (the shared
	// store contract suite).
	Namespace string
	Mount     string
}

// ChatStore is a chat.Store over a fork app. It is safe for concurrent use:
// multi-row invariants are database constraints, and every read-then-write
// decision runs inside one transaction that first locks the row it depends on.
type ChatStore struct {
	app    core.App
	prefix string

	namespace string
	mount     string

	conversations string
	messages      string
	turns         string
	events        string
	proposals     string
	uploads       string

	maxEvents int
	// importMu serializes ImportConversation (see there).
	importMu sync.Mutex
	// lastUploadMS makes consecutive upload timestamps strictly increasing.
	// The stored date keeps millisecond precision, so two uploads stored within
	// one millisecond would otherwise tie and fall back to an arbitrary id
	// order instead of creation order.
	lastUploadMS atomic.Int64
}

var _ chat.Store = (*ChatStore)(nil)
var _ chat.ProposalStore = (*ChatStore)(nil)
var _ chat.UploadStore = (*ChatStore)(nil)
var _ chat.ActiveTurnStore = (*ChatStore)(nil)
var _ chat.RecentMessageStore = (*ChatStore)(nil)
var _ chat.SummaryStore = (*ChatStore)(nil)
var _ chat.ImportStore = (*ChatStore)(nil)

var chatCollectionPrefixPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// NewChatStore provisions (idempotently) the six private chat collections and
// returns a store over them. Provisioning is safe when several app instances
// start against the same database at the same time: a create that loses the
// race re-reads the winner's collection and adopts it instead of failing.
//
// An existing collection with one of the expected names is only adopted when
// it is a System base collection with nil API rules, the exact expected field
// types and the exact expected indexes; anything else fails with an error
// naming the collection, so startup never silently takes over or alters an
// application's own collection. An installation from a release with only the
// four v1.1 collections gains the proposal and upload collections on its next
// start, and a v1.2 install with proposals gains uploads the same way.
func NewChatStore(app core.App, opts ChatStoreOptions) (*ChatStore, error) {
	if app == nil {
		return nil, errors.New("chat store: nil app")
	}
	if (opts.Namespace == "") != (opts.Mount == "") {
		return nil, errors.New("chat store: namespace and mount must be set together (both empty means an unscoped store)")
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultChatCollectionPrefix
	}
	if !chatCollectionPrefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("chat store: collection prefix %q must match %s", prefix, chatCollectionPrefixPattern)
	}
	maxEvents := opts.MaxEventsPerTurn
	if maxEvents <= 0 {
		maxEvents = chat.DefaultMaxEventsPerTurn
	}
	s := &ChatStore{
		app:           app,
		prefix:        prefix,
		namespace:     opts.Namespace,
		mount:         opts.Mount,
		conversations: prefix + "Conversations",
		messages:      prefix + "Messages",
		turns:         prefix + "Turns",
		events:        prefix + "Events",
		proposals:     prefix + "Proposals",
		uploads:       prefix + "Uploads",
		maxEvents:     maxEvents,
	}
	for _, name := range s.Collections() {
		if len(name) > chatCollectionNameMaxLen {
			return nil, fmt.Errorf("chat store: collection name %q exceeds %d characters; use a shorter prefix", name, chatCollectionNameMaxLen)
		}
	}
	for _, spec := range s.specs() {
		for _, idx := range spec.indexes {
			if len(idx.name) > 63 {
				return nil, fmt.Errorf("chat store: index name %q exceeds 63 characters; use a shorter collection prefix", idx.name)
			}
		}
	}
	for _, spec := range s.specs() {
		if _, err := provisionChatCollection(app, spec); err != nil {
			return nil, err
		}
	}
	// A concurrent instance may have created the collections while this app
	// was bootstrapping, so its cached collection list can miss them. Store
	// lookups go through the cache (like every other PocketBase record query),
	// so refresh it once the schema is known to exist.
	if err := app.ReloadCachedCollections(); err != nil {
		return nil, fmt.Errorf("chat store: reload collections cache: %w", err)
	}
	return s, nil
}

// Collections returns the provisioned collection names in a stable order
// (conversations, messages, turns, events, proposals, uploads).
func (s *ChatStore) Collections() []string {
	return []string{s.conversations, s.messages, s.turns, s.events, s.proposals, s.uploads}
}

// --- provisioning ---

type chatIndexSpec struct {
	name    string
	unique  bool
	columns string
}

type chatCollectionSpec struct {
	name    string
	fields  func() []core.Field
	indexes []chatIndexSpec
	// addFields names fields an older, otherwise compatible collection may be
	// missing. adoptChatCollection adds them in place.
	addFields []string
}

func (s chatCollectionSpec) allowsAdd(name string) bool {
	for _, allowed := range s.addFields {
		if allowed == name {
			return true
		}
	}
	return false
}

// chatTextField returns a bounded, schema-protected text field.
func chatTextField(name string, max int, system bool) *core.TextField {
	return &core.TextField{Name: name, Max: max, System: system}
}

func chatOwnerTextFields() []core.Field {
	return []core.Field{
		chatTextField(chatFieldOwnerKey, chatMaxTextLen, true),
		chatTextField(chatFieldNamespace, chatMaxTextLen, true),
		chatTextField(chatFieldMount, chatMaxTextLen, true),
		chatTextField(chatFieldAuthCollection, chatMaxTextLen, true),
		chatTextField(chatFieldUserID, chatMaxTextLen, true),
		chatTextField(chatFieldTenant, chatMaxTextLen, true),
	}
}

// chatJSONTextField stores a JSON payload as raw text. See the package comment
// for why it is not a core.JSONField.
func chatJSONTextField(name string) *core.TextField {
	return chatTextField(name, chatJSONFieldMaxChars, true)
}

func (s *ChatStore) specs() []chatCollectionSpec {
	return []chatCollectionSpec{
		{
			name: s.conversations,
			fields: func() []core.Field {
				fields := chatOwnerTextFields()
				return append(fields,
					chatTextField(chatFieldTitle, chatMaxTextLen, true),
					chatTextField(chatFieldTarget, chatMaxTextLen, true),
					&core.DateField{Name: chatFieldCreatedAt, System: true},
					&core.DateField{Name: chatFieldUpdatedAt, System: true},
					chatJSONTextField(chatFieldContextSummary),
					chatTextField(chatFieldImportSource, chatMaxTextLen, true),
				)
			},
			// v1.4 adds the conversation summary and the import source. Older
			// stores gain them in place.
			addFields: []string{chatFieldContextSummary, chatFieldImportSource},
			indexes: []chatIndexSpec{
				// The id tie-break is applied by the ORDER BY, not by the
				// index.
				{name: "idx_" + s.conversations + "_owner_created", columns: chatFieldOwnerKey + ", " + chatFieldCreatedAt},
			},
		},
		{
			name: s.messages,
			fields: func() []core.Field {
				return []core.Field{
					chatTextField(chatFieldOwnerKey, chatMaxTextLen, true),
					chatTextField(chatFieldConversation, chatMaxTextLen, true),
					chatTextField(chatFieldTurn, chatMaxTextLen, true),
					chatTextField(chatFieldRole, chatMaxTextLen, true),
					chatJSONTextField(chatFieldParts),
					&core.DateField{Name: chatFieldCreatedAt, System: true},
				}
			},
			indexes: []chatIndexSpec{
				{name: "idx_" + s.messages + "_owner_conversation", columns: chatFieldOwnerKey + ", " + chatFieldConversation},
			},
		},
		{
			name: s.turns,
			// v1.3 adds the provider to a turn. Older fork stores gain it in place.
			addFields: []string{chatFieldProvider, chatFieldEffort},
			fields: func() []core.Field {
				fields := chatOwnerTextFields()
				return append(fields,
					chatTextField(chatFieldConversation, chatMaxTextLen, true),
					chatTextField(chatFieldState, chatMaxTextLen, true),
					chatTextField(chatFieldIdempotencyKey, chatIdempotencyKeyFieldMax, true),
					chatTextField(chatFieldIdempotencySlot, chatSlotMaxLen, true),
					chatTextField(chatFieldActiveSlot, chatSlotMaxLen, true),
					chatTextField(chatFieldProvider, chatMaxTextLen, true),
					chatTextField(chatFieldModel, chatMaxTextLen, true),
					chatTextField(chatFieldEffort, chatMaxTextLen, true),
					&core.NumberField{Name: chatFieldLastSeq, OnlyInt: true, System: true},
					&core.NumberField{Name: chatFieldDroppedBefore, OnlyInt: true, System: true},
					chatJSONTextField(chatFieldError),
					&core.DateField{Name: chatFieldCreatedAt, System: true},
					&core.DateField{Name: chatFieldUpdatedAt, System: true},
				)
			},
			indexes: []chatIndexSpec{
				// One active turn per conversation: the column holds the
				// conversation id until the turn ends and a per-turn unique
				// placeholder afterwards, so no partial index is needed.
				{name: "idx_" + s.turns + "_active", unique: true, columns: chatFieldActiveSlot},
				// One turn per (owner, conversation, client key). Keyless
				// turns carry a per-turn placeholder, so they never collide.
				{name: "idx_" + s.turns + "_idempotency", unique: true, columns: chatFieldIdempotencySlot},
				{name: "idx_" + s.turns + "_owner_conversation", columns: chatFieldOwnerKey + ", " + chatFieldConversation},
			},
		},
		{
			name: s.events,
			fields: func() []core.Field {
				return []core.Field{
					chatTextField(chatFieldOwnerKey, chatMaxTextLen, true),
					chatTextField(chatFieldConversation, chatMaxTextLen, true),
					chatTextField(chatFieldTurn, chatMaxTextLen, true),
					&core.NumberField{Name: chatFieldSeq, OnlyInt: true, System: true},
					chatTextField(chatFieldType, chatMaxTextLen, true),
					chatTextField(chatFieldVersion, chatMaxTextLen, true),
					chatJSONTextField(chatFieldData),
					&core.DateField{Name: chatFieldAt, System: true},
				}
			},
			indexes: []chatIndexSpec{
				// Sequences are per turn and may never fork.
				{name: "idx_" + s.events + "_turn_seq", unique: true, columns: chatFieldTurn + ", " + chatFieldSeq},
				{name: "idx_" + s.events + "_owner_turn", columns: chatFieldOwnerKey + ", " + chatFieldTurn},
			},
		},
		{
			name: s.proposals,
			fields: func() []core.Field {
				fields := chatOwnerTextFields()
				return append(fields,
					chatTextField(chatFieldProposalConversation, chatMaxTextLen, true),
					chatTextField(chatFieldProposalTurn, chatMaxTextLen, true),
					chatTextField(chatFieldCallID, chatMaxTextLen, true),
					chatTextField(chatFieldTool, chatMaxTextLen, true),
					chatTextField(chatFieldToolFingerprint, chatMaxTextLen, true),
					chatTextField(chatFieldManifestRevision, chatMaxTextLen, true),
					chatTextField(chatFieldLocation, chatMaxTextLen, true),
					chatTextField(chatFieldTarget, chatMaxTextLen, true),
					chatJSONTextField(chatFieldArguments),
					chatTextField(chatFieldArgumentsHash, chatMaxTextLen, true),
					chatTextField(chatFieldSummary, chatMaxTextLen, true),
					chatTextField(chatFieldState, chatMaxTextLen, true),
					&core.NumberField{Name: chatFieldRevision, OnlyInt: true, System: true},
					chatJSONTextField(chatFieldResult),
					chatTextField(chatFieldFailure, chatMaxTextLen, true),
					chatTextField(chatFieldEditorRevision, chatMaxTextLen, true),
					chatJSONTextField(chatFieldHunks),
					&core.DateField{Name: chatFieldExpiresAt, System: true},
					&core.DateField{Name: chatFieldDecidedAt, System: true},
					&core.DateField{Name: chatFieldCreatedAt, System: true},
					&core.DateField{Name: chatFieldUpdatedAt, System: true},
					chatJSONTextField(chatFieldWarnings),
				)
			},
			// v1.4 adds proposal warnings. Older stores gain them in place.
			addFields: []string{chatFieldWarnings},
			indexes: []chatIndexSpec{
				// The per-conversation list order is served by this index; the
				// id tie-break is applied by the ORDER BY, as in the other
				// collections.
				{name: "idx_" + s.proposals + "_owner_conversation", columns: chatFieldOwnerKey + ", " + chatFieldProposalConversation},
				// The expiry sweep and restart recovery scan by state; the
				// second index covers the deadline filter.
				{name: "idx_" + s.proposals + "_state", columns: chatFieldState},
				{name: "idx_" + s.proposals + "_state_expires", columns: chatFieldState + ", " + chatFieldExpiresAt},
			},
		},
		{
			name: s.uploads,
			fields: func() []core.Field {
				fields := chatOwnerTextFields()
				return append(fields,
					chatTextField(chatFieldUploadConversation, chatMaxTextLen, true),
					chatTextField(chatFieldUploadName, chatMaxTextLen, true),
					chatTextField(chatFieldUploadMIME, chatMaxTextLen, true),
					chatTextField(chatFieldUploadKind, chatMaxTextLen, true),
					&core.NumberField{Name: chatFieldUploadSize, OnlyInt: true, System: true},
					chatTextField(chatFieldUploadSHA256, chatMaxTextLen, true),
					// A protected file: the store never forms a public URL or a
					// file token for it. The bound is a safety net only; the
					// HTTP layer enforces the real per-request limit.
					&core.FileField{Name: chatFieldUploadData, MaxSelect: 1, MaxSize: chatMaxUploadFileBytes, Protected: true, System: true},
					&core.DateField{Name: chatFieldCreatedAt, System: true},
					&core.DateField{Name: chatFieldUpdatedAt, System: true},
				)
			},
			indexes: []chatIndexSpec{
				// The newest-first list, the per-conversation count and the
				// delete cascade all scan by (owner, conversation).
				{name: "idx_" + s.uploads + "_owner_conversation", columns: chatFieldOwnerKey + ", " + chatFieldUploadConversation + ", " + chatFieldCreatedAt},
			},
		},
	}
}

func provisionChatCollection(app core.App, spec chatCollectionSpec) (bool, error) {
	// Collections are provisioned one at a time rather than in one enclosing
	// transaction: on PostgreSQL a concurrent create that loses the name race aborts the whole transaction,
	// which would discard the collections this instance had already created.
	// Each create is idempotent on its own, so a partial pass is simply
	// completed by re-reading (or by the next start).
	existing, err := app.FindCollectionByNameOrId(spec.name)
	if err == nil && existing != nil {
		return adoptChatCollection(app, spec, existing)
	}
	col := core.NewBaseCollection(spec.name)
	// A System collection is visible to every instance regardless of its
	// collection scope, exactly like the fork's own private system
	// collections. Nil API rules are PocketBase's "superusers only" state.
	col.System = true
	for _, f := range spec.fields() {
		col.Fields.Add(f)
	}
	for _, idx := range spec.indexes {
		col.AddIndex(idx.name, idx.unique, idx.columns, "")
	}
	if err := app.Save(col); err != nil {
		// Another instance may have created the collection between our read
		// and our create. Re-read: if it is there now, adopt it, retrying a
		// few times while the winner's commit becomes visible.
		for attempt := 0; attempt < 5; attempt++ {
			if again, ferr := app.FindCollectionByNameOrId(spec.name); ferr == nil && again != nil {
				return adoptChatCollection(app, spec, again)
			}
			if !chatAlreadyExists(err) {
				break
			}
			time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
		}
		return false, fmt.Errorf("chat store: provision collection %q: %w", spec.name, err)
	}
	return true, nil
}

// adoptChatCollection checks an existing collection and repairs a collection
// whose table is missing.
func adoptChatCollection(app core.App, spec chatCollectionSpec, existing *core.Collection) (bool, error) {
	if !app.HasTable(existing.Name) {
		// The collection record exists but its table does not (a table
		// dropped by hand, say). Recreate the missing table from the metadata rather than
		// failing startup.
		if err := app.SyncRecordTableSchema(existing, nil); err != nil {
			return false, fmt.Errorf("chat store: create missing table for collection %q: %w", spec.name, err)
		}
	}
	if err := checkChatCollectionCompatible(spec, existing); err != nil {
		return false, err
	}
	added := false
	for _, want := range spec.fields() {
		got := existing.Fields.GetByName(want.GetName())
		if got == nil && spec.allowsAdd(want.GetName()) {
			existing.Fields.Add(want)
			added = true
			continue
		}
		// A text field's maximum only ever rises (v1.4 raised the JSON
		// payload fields from 1 MiB to 4 MiB characters). The column type
		// doesn't change: TEXT either way.
		if gotText, ok := got.(*core.TextField); ok {
			if wantText, ok := want.(*core.TextField); ok && gotText.Max > 0 && wantText.Max > gotText.Max {
				gotText.Max = wantText.Max
				added = true
			}
		}
	}
	if added {
		if err := app.Save(existing); err != nil {
			return false, fmt.Errorf("chat store: upgrade collection %q: %w", spec.name, err)
		}
	}
	return added, nil
}

// chatAlreadyExists reports an "object already exists" failure from a
// concurrent create. It is only used to decide whether re-reading is worth a
// retry, never to classify a store operation.
func chatAlreadyExists(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "duplicate entry") ||
		strings.Contains(msg, "value must be unique") ||
		strings.Contains(msg, "sqlstate 23505") ||
		strings.Contains(msg, "error 1062")
}

// checkChatCollectionCompatible refuses to adopt a collection that is not
// exactly the private system schema this store owns.
func checkChatCollectionCompatible(spec chatCollectionSpec, existing *core.Collection) error {
	conflict := func(format string, args ...any) error {
		return fmt.Errorf("chat store: conflicting existing schema for collection %q: %s (remove it or move it under a different name before enabling chat)",
			spec.name, fmt.Sprintf(format, args...))
	}
	if !existing.IsBase() {
		return conflict("expected a base collection, got %q", existing.Type)
	}
	if !existing.System {
		return conflict("must be a System collection so every instance can reach it")
	}
	if existing.ListRule != nil || existing.ViewRule != nil || existing.CreateRule != nil || existing.UpdateRule != nil || existing.DeleteRule != nil {
		return conflict("API rules must all be nil so the collection is superuser-only")
	}
	for _, want := range spec.fields() {
		got := existing.Fields.GetByName(want.GetName())
		if got == nil {
			if spec.allowsAdd(want.GetName()) {
				continue
			}
			return conflict("missing field %q (%s)", want.GetName(), want.Type())
		}
		if got.Type() != want.Type() {
			return conflict("field %q has type %q, expected %q", want.GetName(), got.Type(), want.Type())
		}
	}
	for _, want := range spec.indexes {
		raw := existing.GetIndex(want.name)
		if raw == "" {
			return conflict("missing index %q (%s)", want.name, want.columns)
		}
		got := dbutils.ParseIndex(raw)
		if got.Unique != want.unique {
			return conflict("index %q unique = %v, expected %v", want.name, got.Unique, want.unique)
		}
		if strings.TrimSpace(got.Where) != "" {
			return conflict("index %q must not be a partial index", want.name)
		}
		if !chatSameColumns(got.Columns, want.columns) {
			return conflict("index %q has columns %v, expected %s", want.name, chatIndexColumns(got), want.columns)
		}
	}
	return nil
}

func chatSameColumns(got []dbutils.IndexColumn, want string) bool {
	wantCols := strings.Split(want, ",")
	if len(got) != len(wantCols) {
		return false
	}
	for i, c := range wantCols {
		if !strings.EqualFold(strings.TrimSpace(got[i].Name), strings.TrimSpace(c)) {
			return false
		}
	}
	return true
}

func chatIndexColumns(idx dbutils.Index) []string {
	out := make([]string, 0, len(idx.Columns))
	for _, c := range idx.Columns {
		out = append(out, c.Name)
	}
	return out
}

// --- chat.Store implementation ---

func chatNow() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

// chatTime normalizes a caller- or store-supplied timestamp to the precision
// the date fields store, so a value read back from the database is exactly the
// value the API returned.
func chatTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Millisecond)
}

func chatDateParam(t time.Time) string { return t.UTC().Format(types.DefaultDateLayout) }

func chatOwnerParams(owner chat.OwnerKey) dbx.Params {
	return dbx.Params{chatFieldOwnerKey: owner.String()}
}

func chatSetOwner(rec *core.Record, owner chat.OwnerKey) {
	rec.Set(chatFieldOwnerKey, owner.String())
	rec.Set(chatFieldNamespace, owner.Namespace)
	rec.Set(chatFieldMount, owner.Mount)
	rec.Set(chatFieldAuthCollection, owner.AuthCollectionID)
	rec.Set(chatFieldUserID, owner.UserID)
	rec.Set(chatFieldTenant, owner.Tenant)
}

func chatOwnerFrom(rec *core.Record) chat.OwnerKey {
	return chat.OwnerKey{
		Namespace:        rec.GetString(chatFieldNamespace),
		Mount:            rec.GetString(chatFieldMount),
		AuthCollectionID: rec.GetString(chatFieldAuthCollection),
		UserID:           rec.GetString(chatFieldUserID),
		Tenant:           rec.GetString(chatFieldTenant),
	}
}

// chatRawJSON reads a JSON payload stored as text. It returns nil for an
// empty column and always a fresh copy, so callers cannot alias stored bytes.
func chatRawJSON(rec *core.Record, name string) json.RawMessage {
	s := rec.GetString(name)
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return json.RawMessage(append([]byte(nil), s...))
}

func chatSetJSON(rec *core.Record, name string, raw json.RawMessage) error {
	if len(raw) == 0 {
		rec.Set(name, "")
		return nil
	}
	if limit := chatMaxJSONBytes; len(raw) > limit {
		return fmt.Errorf("chat store: %s payload is %d bytes, over this database's limit of %d", name, len(raw), limit)
	}
	rec.Set(name, string(raw))
	return nil
}

func chatConversationFrom(rec *core.Record) chat.Conversation {
	return chat.Conversation{
		ID:        rec.Id,
		Owner:     chatOwnerFrom(rec),
		Title:     rec.GetString(chatFieldTitle),
		Target:    rec.GetString(chatFieldTarget),
		CreatedAt: rec.GetDateTime(chatFieldCreatedAt).Time(),
		UpdatedAt: rec.GetDateTime(chatFieldUpdatedAt).Time(),
	}
}

func chatMessageFrom(rec *core.Record) chat.Message {
	m := chat.Message{
		ID:             rec.Id,
		ConversationID: rec.GetString(chatFieldConversation),
		TurnID:         rec.GetString(chatFieldTurn),
		Role:           rec.GetString(chatFieldRole),
		CreatedAt:      rec.GetDateTime(chatFieldCreatedAt).Time(),
	}
	if raw := chatRawJSON(rec, chatFieldParts); len(raw) > 0 {
		var parts []chat.Part
		if err := json.Unmarshal(raw, &parts); err == nil {
			m.Parts = parts
		}
	}
	return m
}

func chatTurnFrom(rec *core.Record) chat.Turn {
	// idempotencySlot and activeSlot are storage invariants only; the client
	// key returned to the caller always comes from idempotencyKey, which never
	// holds a placeholder.
	t := chat.Turn{
		ID:             rec.Id,
		ConversationID: rec.GetString(chatFieldConversation),
		Owner:          chatOwnerFrom(rec),
		State:          chat.TurnState(rec.GetString(chatFieldState)),
		IdempotencyKey: rec.GetString(chatFieldIdempotencyKey),
		Provider:       rec.GetString(chatFieldProvider),
		Model:          rec.GetString(chatFieldModel),
		Effort:         rec.GetString(chatFieldEffort),
		CreatedAt:      rec.GetDateTime(chatFieldCreatedAt).Time(),
		UpdatedAt:      rec.GetDateTime(chatFieldUpdatedAt).Time(),
	}
	if raw := chatRawJSON(rec, chatFieldError); len(raw) > 0 && string(raw) != "null" {
		var failure chat.TurnFailed
		if err := json.Unmarshal(raw, &failure); err == nil {
			t.Error = &failure
		}
	}
	return t
}

func chatEventFrom(rec *core.Record) chat.Event {
	return chat.Event{
		Version:        rec.GetString(chatFieldVersion),
		Seq:            rec.GetInt64(chatFieldSeq),
		ConversationID: rec.GetString(chatFieldConversation),
		TurnID:         rec.GetString(chatFieldTurn),
		Type:           chat.EventType(rec.GetString(chatFieldType)),
		Data:           chatRawJSON(rec, chatFieldData),
		At:             rec.GetDateTime(chatFieldAt).Time(),
	}
}

// chatTerminalSlot is the activeSlot value of a terminal turn: unique per turn,
// so a plain UNIQUE index can be used instead of a partial one.
func chatTerminalSlot(turnID string) string { return "done:" + turnID }

// chatKeylessSlot is the idempotencySlot value of a turn whose client sent no
// Idempotency-Key: unique per turn and never mistaken for a client key.
func chatKeylessSlot(turnID string) string { return "none:" + turnID }

// chatKeySlot is the fixed-length uniqueness digest of one client key. The
// hash covers the full owner key, so two apps that share the database can
// never collide, and it is derived from values that are already validated.
func chatKeySlot(owner chat.OwnerKey, conversationID, key string) string {
	h := sha256.New()
	h.Write([]byte(owner.String()))
	h.Write([]byte{0})
	h.Write([]byte(conversationID))
	h.Write([]byte{0})
	h.Write([]byte(key))
	return "key:" + hex.EncodeToString(h.Sum(nil))
}

// chatFindOwned resolves a record only when it belongs to owner. A record that
// exists under another owner is reported exactly like a missing one.
func chatFindOwned(app core.App, collection string, owner chat.OwnerKey, id string) (*core.Record, error) {
	params := chatOwnerParams(owner)
	params["id"] = id
	rec, err := app.FindFirstRecordByFilter(collection, "id = {:id} && "+chatFieldOwnerKey+" = {:ownerKey}", params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, chat.ErrNotFound
		}
		return nil, err
	}
	return rec, nil
}

// chatTouchConversation moves a conversation's updatedAt to at, so the list
// shows when it last had activity (a new turn), not only when it was renamed.
func chatTouchConversation(ctx context.Context, tx core.App, collection string, owner chat.OwnerKey, id string, at time.Time) error {
	rec, err := chatFindOwned(tx, collection, owner, id)
	if err != nil {
		return err
	}
	rec.Set(chatFieldUpdatedAt, at)
	return tx.SaveWithContext(ctx, rec)
}

// chatFirst returns ErrNotFound when the filter matches nothing.
func chatFirst(app core.App, collection, filter string, params dbx.Params) (*core.Record, error) {
	rec, err := app.FindFirstRecordByFilter(collection, filter, params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, chat.ErrNotFound
		}
		return nil, err
	}
	return rec, nil
}

func chatCollection(app core.App, name string) (*core.Collection, error) {
	col, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		return nil, fmt.Errorf("chat store: collection %q is unavailable: %w", name, err)
	}
	if col == nil {
		return nil, fmt.Errorf("chat store: collection %q is unavailable", name)
	}
	if !col.System {
		return nil, fmt.Errorf("chat store: collection %q is not a System collection and may be hidden from this instance", name)
	}
	return col, nil
}

// chatLockOwned locks one owned row for the rest of the transaction and
// reports chat.ErrNotFound when it does not exist or belongs to somebody else.
//
// Locking is what makes a read-then-write decision safe on PostgreSQL:
// two app instances sharing one database run these transactions concurrently,
// and the loser of a lock waits here and then reads the winner's committed
// state. The table names are package constants, never user input, and every
// value is bound.
func chatLockOwned(tx core.App, collection string, owner chat.OwnerKey, id string) error {
	var got string
	err := tx.NonconcurrentDB().
		NewQuery("SELECT [[id]] FROM {{" + collection + "}} WHERE [[id]] = {:id} AND [[ownerKey]] = {:ownerKey} FOR UPDATE").
		Bind(dbx.Params{"id": id, chatFieldOwnerKey: owner.String()}).
		Row(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return chat.ErrNotFound
	}
	return err
}

// chatLockAndFind locks a row and then reads it again, so the record reflects
// everything committed while the lock was being acquired.
func chatLockAndFind(tx core.App, collection string, owner chat.OwnerKey, id string) (*core.Record, error) {
	if err := chatLockOwned(tx, collection, owner, id); err != nil {
		return nil, err
	}
	return chatFindOwned(tx, collection, owner, id)
}

// chatLockConversation serializes turn creation, messages, rename and delete
// for one conversation without locking an entire collection.
func (s *ChatStore) chatLockConversation(tx core.App, owner chat.OwnerKey, id string) error {
	return chatLockOwned(tx, s.conversations, owner, id)
}

// chatUniqueViolation reports a database uniqueness failure. It recognises the
// PostgreSQL driver error, the validation error PocketBase derives
// from a single-column unique index, and it never treats NOT NULL, foreign key
// or check violations as unique.
func chatUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Negative first: these can share words ("constraint") with the unique
	// messages and must never trigger a retry or a conflict mapping.
	for _, notUnique := range []string{
		"sqlstate 23502", "not-null constraint", "not null constraint",
		"sqlstate 23503", "foreign key constraint",
		"sqlstate 23514", "check constraint",
	} {
		if strings.Contains(msg, notUnique) {
			return false
		}
	}
	for _, unique := range []string{
		"sqlstate 23505", // PostgreSQL SQLSTATE
		"duplicate key value violates unique constraint", // PostgreSQL
		"value must be unique",                           // PocketBase's normalized index error
		"unique constraint failed",                       // SQLite wording (defensive)
	} {
		if strings.Contains(msg, unique) {
			return true
		}
	}
	return false
}

// chatRetryableTxError reports a failure that aborted the transaction before
// any of our decisions were committed, so re-running the whole transaction is
// safe: a lost unique race, a deadlock or a lock timeout.
func chatRetryableTxError(err error) bool {
	if err == nil {
		return false
	}
	if chatUniqueViolation(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadlock detected") ||
		strings.Contains(msg, "could not serialize access") ||
		strings.Contains(msg, "database is locked")
}

// chatRunTx runs fn in one transaction, retrying a small number of times when
// the transaction lost a race before committing. Every fn must be safe to
// re-run from scratch: it locks its rows again and re-reads them, so a retry
// observes the winner's state instead of replaying a stale decision.
func (s *ChatStore) chatRunTx(fn func(core.App) error) error {
	var err error
	for attempt := 0; attempt < chatMaxTxAttempts; attempt++ {
		err = s.app.RunInTransaction(fn)
		if err == nil || !chatRetryableTxError(err) {
			return err
		}
	}
	return err
}

func (s *ChatStore) CreateConversation(ctx context.Context, c chat.Conversation) (chat.Conversation, error) {
	if err := c.Owner.Validate(); err != nil {
		return chat.Conversation{}, err
	}
	col, err := chatCollection(s.app, s.conversations)
	if err != nil {
		return chat.Conversation{}, err
	}
	rec := core.NewRecord(col)
	if c.ID != "" {
		rec.Set(core.FieldNameId, c.ID)
	}
	chatSetOwner(rec, c.Owner)
	rec.Set(chatFieldTitle, c.Title)
	rec.Set(chatFieldTarget, c.Target)
	created := chatTime(c.CreatedAt)
	if created.IsZero() {
		created = chatNow()
	}
	updated := chatTime(c.UpdatedAt)
	if updated.IsZero() {
		updated = created
	}
	rec.Set(chatFieldCreatedAt, created)
	rec.Set(chatFieldUpdatedAt, updated)
	if err := s.app.SaveWithContext(ctx, rec); err != nil {
		if chatUniqueViolation(err) {
			return chat.Conversation{}, chat.ErrConflict
		}
		return chat.Conversation{}, err
	}
	return chatConversationFrom(rec), nil
}

func (s *ChatStore) GetConversation(_ context.Context, owner chat.OwnerKey, id string) (chat.Conversation, error) {
	rec, err := chatFindOwned(s.app, s.conversations, owner, id)
	if err != nil {
		return chat.Conversation{}, err
	}
	return chatConversationFrom(rec), nil
}

func (s *ChatStore) ListConversations(_ context.Context, owner chat.OwnerKey, target string, page chat.Page) ([]chat.Conversation, string, error) {
	limit := chatPageLimit(page)
	after, err := chatDecodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter := chatFieldOwnerKey + " = {:ownerKey}"
	params := chatOwnerParams(owner)
	if target != "" {
		filter += " && " + chatFieldTarget + " = {:target}"
		params[chatFieldTarget] = target
	}
	if after != nil {
		filter += " && (" + chatFieldCreatedAt + " < {:at} || (" + chatFieldCreatedAt + " = {:at} && id < {:id}))"
		params["at"] = chatDateParam(after.at)
		params["id"] = after.id
	}
	recs, err := s.app.FindRecordsByFilter(s.conversations, filter, "-"+chatFieldCreatedAt+",-id", limit+1, 0, params)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(recs) > limit {
		recs = recs[:limit]
		last := recs[limit-1]
		next = chatEncodeCursor(last.GetDateTime(chatFieldCreatedAt).Time(), last.Id)
	}
	out := make([]chat.Conversation, 0, len(recs))
	for _, rec := range recs {
		out = append(out, chatConversationFrom(rec))
	}
	return out, next, nil
}

func (s *ChatStore) RenameConversation(ctx context.Context, owner chat.OwnerKey, id, title string) (chat.Conversation, error) {
	var out chat.Conversation
	err := s.chatRunTx(func(tx core.App) error {
		rec, err := chatLockAndFind(tx, s.conversations, owner, id)
		if err != nil {
			return err
		}
		rec.Set(chatFieldTitle, title)
		rec.Set(chatFieldUpdatedAt, chatNow())
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		out = chatConversationFrom(rec)
		return nil
	})
	if err != nil {
		return chat.Conversation{}, err
	}
	return out, nil
}

func (s *ChatStore) DeleteConversation(ctx context.Context, owner chat.OwnerKey, id string) error {
	return s.chatRunTx(func(tx core.App) error {
		// The conversation lock serializes this delete with CreateTurn and
		// AppendMessage, which take the same lock before they touch the
		// conversation's rows.
		rec, err := chatLockAndFind(tx, s.conversations, owner, id)
		if err != nil {
			return err
		}
		params := chatOwnerParams(owner)
		params[chatFieldConversation] = id
		if _, err := chatFirst(tx, s.turns,
			chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldActiveSlot+" = {:conversation}",
			params); err == nil {
			return chat.ErrActiveTurn
		} else if !errors.Is(err, chat.ErrNotFound) {
			return err
		}
		// An executing proposal may still produce a server effect, so its
		// conversation cannot disappear underneath it. Locking each proposal
		// row serializes this check with a concurrent decision, which locks the
		// same row before it may move the proposal to executing. An active turn
		// wins the refusal above because it is the reason the conversation is
		// busy.
		if err := s.chatRefuseExecutingProposals(ctx, tx, owner, id); err != nil {
			return err
		}
		for _, rows := range []struct{ collection, conversation string }{
			{s.proposals, chatFieldProposalConversation},
			{s.events, chatFieldConversation},
			{s.messages, chatFieldConversation},
			{s.turns, chatFieldConversation},
		} {
			if err := chatDeleteConversationRows(tx, rows.collection, rows.conversation, owner, id); err != nil {
				return err
			}
		}
		// Uploads are removed one record at a time, not with a bulk DELETE:
		// the app deletes a record's files as part of the record delete, so a
		// bulk statement would remove the rows and leave the bytes behind. The
		// conversation lock taken above serializes this with CreateUpload,
		// which takes the same lock before it inserts, so no upload can slip in
		// after the scan. Only this conversation's upload records are touched.
		if err := chatDeleteConversationUploads(ctx, tx, s.uploads, owner, id); err != nil {
			return err
		}
		return tx.DeleteWithContext(ctx, rec)
	})
}

// chatRefuseExecutingProposals locks one conversation's proposals and reports
// chat.ErrConflict while any of them is executing.
func (s *ChatStore) chatRefuseExecutingProposals(ctx context.Context, tx core.App, owner chat.OwnerKey, conversationID string) error {
	params := chatOwnerParams(owner)
	params[chatFieldProposalConversation] = conversationID
	candidates, err := tx.FindRecordsByFilter(s.proposals,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldProposalConversation+" = {:conversationId}",
		"id", 0, 0, params)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		rec, err := chatLockAndFind(tx, s.proposals, owner, candidate.Id)
		if errors.Is(err, chat.ErrNotFound) {
			continue // deleted while we scanned
		}
		if err != nil {
			return err
		}
		if chat.ProposalState(rec.GetString(chatFieldState)) == chat.ProposalStateExecuting {
			return chat.ErrConflict
		}
	}
	return nil
}

func withParam(params dbx.Params, key string, value any) dbx.Params {
	out := dbx.Params{}
	for k, v := range params {
		out[k] = v
	}
	out[key] = value
	return out
}

// chatDeleteConversationRows removes every row of one collection belonging to
// one conversation of one owner. The table name and the conversation column
// come from the provisioned schema (never from user input); the values are
// bound parameters.
func chatDeleteConversationRows(app core.App, collection, conversationColumn string, owner chat.OwnerKey, conversationID string) error {
	_, err := app.NonconcurrentDB().
		NewQuery("DELETE FROM {{" + collection + "}} WHERE [[ownerKey]] = {:ownerKey} AND [[" + conversationColumn + "]] = {:conversation}").
		Bind(dbx.Params{chatFieldOwnerKey: owner.String(), chatFieldConversation: conversationID}).
		Execute()
	if err != nil {
		return fmt.Errorf("chat store: delete %s rows: %w", collection, err)
	}
	return nil
}

func (s *ChatStore) AppendMessage(ctx context.Context, owner chat.OwnerKey, m chat.Message) (chat.Message, error) {
	var out chat.Message
	err := s.chatRunTx(func(tx core.App) error {
		if err := s.chatLockConversation(tx, owner, m.ConversationID); err != nil {
			return err
		}
		if m.TurnID != "" {
			turn, err := chatFindOwned(tx, s.turns, owner, m.TurnID)
			if err != nil {
				return err
			}
			if turn.GetString(chatFieldConversation) != m.ConversationID {
				return chat.ErrNotFound
			}
		}
		col, err := chatCollection(tx, s.messages)
		if err != nil {
			return err
		}
		rec := core.NewRecord(col)
		if m.ID != "" {
			rec.Set(core.FieldNameId, m.ID)
		}
		chatSetOwner(rec, owner)
		rec.Set(chatFieldConversation, m.ConversationID)
		rec.Set(chatFieldTurn, m.TurnID)
		rec.Set(chatFieldRole, m.Role)
		if len(m.Parts) > 0 {
			raw, err := json.Marshal(m.Parts)
			if err != nil {
				return fmt.Errorf("chat store: marshal message parts: %w", err)
			}
			if err := chatSetJSON(rec, chatFieldParts, raw); err != nil {
				return err
			}
		} else if err := chatSetJSON(rec, chatFieldParts, nil); err != nil {
			return err
		}
		created := chatTime(m.CreatedAt)
		if created.IsZero() {
			created = chatNow()
		}
		rec.Set(chatFieldCreatedAt, created)
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		out = chatMessageFrom(rec)
		return nil
	})
	if err != nil {
		return chat.Message{}, err
	}
	return out, nil
}

// ActiveTurn implements chat.ActiveTurnStore through the indexed active slot,
// without reading the transcript.
func (s *ChatStore) ActiveTurn(_ context.Context, owner chat.OwnerKey, conversationID string) (chat.Turn, bool, error) {
	if _, err := chatFindOwned(s.app, s.conversations, owner, conversationID); err != nil {
		return chat.Turn{}, false, err
	}
	params := chatOwnerParams(owner)
	params[chatFieldConversation] = conversationID
	rec, err := chatFirst(s.app, s.turns,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldActiveSlot+" = {:conversation}",
		params)
	if errors.Is(err, chat.ErrNotFound) {
		return chat.Turn{}, false, nil
	}
	if err != nil {
		return chat.Turn{}, false, err
	}
	return chatTurnFrom(rec), true, nil
}

// RecentMessages implements chat.RecentMessageStore: the newest limit messages,
// read newest first and returned in ascending order.
func (s *ChatStore) RecentMessages(_ context.Context, owner chat.OwnerKey, conversationID string, limit int) ([]chat.Message, error) {
	if _, err := chatFindOwned(s.app, s.conversations, owner, conversationID); err != nil {
		return nil, err
	}
	params := chatOwnerParams(owner)
	params[chatFieldConversation] = conversationID
	recs, err := s.app.FindRecordsByFilter(s.messages,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation}",
		"-"+chatFieldCreatedAt+",-id", limit, 0, params)
	if err != nil {
		return nil, err
	}
	out := make([]chat.Message, len(recs))
	for i, rec := range recs {
		out[len(recs)-1-i] = chatMessageFrom(rec)
	}
	return out, nil
}

func (s *ChatStore) ListMessages(_ context.Context, owner chat.OwnerKey, conversationID string, page chat.Page) ([]chat.Message, string, error) {
	if _, err := chatFindOwned(s.app, s.conversations, owner, conversationID); err != nil {
		return nil, "", err
	}
	limit := chatPageLimit(page)
	after, err := chatDecodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter := chatFieldOwnerKey + " = {:ownerKey} && " + chatFieldConversation + " = {:conversation}"
	params := chatOwnerParams(owner)
	params[chatFieldConversation] = conversationID
	if after != nil {
		filter += " && (" + chatFieldCreatedAt + " > {:at} || (" + chatFieldCreatedAt + " = {:at} && id > {:id}))"
		params["at"] = chatDateParam(after.at)
		params["id"] = after.id
	}
	recs, err := s.app.FindRecordsByFilter(s.messages, filter, chatFieldCreatedAt+",id", limit+1, 0, params)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(recs) > limit {
		recs = recs[:limit]
		last := recs[limit-1]
		next = chatEncodeCursor(last.GetDateTime(chatFieldCreatedAt).Time(), last.Id)
	}
	out := make([]chat.Message, 0, len(recs))
	for _, rec := range recs {
		out = append(out, chatMessageFrom(rec))
	}
	return out, next, nil
}

func (s *ChatStore) CreateTurn(ctx context.Context, owner chat.OwnerKey, t chat.Turn) (chat.Turn, bool, error) {
	if err := owner.Validate(); err != nil {
		return chat.Turn{}, false, err
	}
	if len(t.IdempotencyKey) > chatMaxIdempotencyKeyBytes {
		return chat.Turn{}, false, fmt.Errorf("chat store: idempotency key is %d bytes, the limit is %d: %w", len(t.IdempotencyKey), chatMaxIdempotencyKeyBytes, chat.ErrConflict)
	}
	col, err := chatCollection(s.app, s.turns)
	if err != nil {
		return chat.Turn{}, false, err
	}
	// The record id is needed before the insert because both uniqueness
	// placeholders embed it. Generating it here uses the same generator a
	// save would, and an explicit caller id is kept as-is.
	turnID := t.ID
	if turnID == "" {
		turnID = core.GenerateDefaultRandomId()
	}
	slot := chatKeylessSlot(turnID)
	if t.IdempotencyKey != "" {
		slot = chatKeySlot(owner, t.ConversationID, t.IdempotencyKey)
	}

	var (
		out     chat.Turn
		created bool
	)
	err = s.app.RunInTransaction(func(tx core.App) error {
		out, created = chat.Turn{}, false
		// Lock the conversation first: it serializes turn creation against
		// delete and makes the active-turn pre-check meaningful.
		if err := s.chatLockConversation(tx, owner, t.ConversationID); err != nil {
			return err
		}
		params := chatOwnerParams(owner)
		params[chatFieldConversation] = t.ConversationID

		if t.IdempotencyKey != "" {
			existing, err := chatFirst(tx, s.turns,
				chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldIdempotencySlot+" = {:slot}",
				withParam(params, "slot", slot))
			switch {
			case err == nil:
				out, created = chatTurnFrom(existing), false
				return nil
			case errors.Is(err, chat.ErrNotFound):
			default:
				return err
			}
		}
		if _, err := chatFirst(tx, s.turns,
			chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldActiveSlot+" = {:conversation}",
			params); err == nil {
			return chat.ErrActiveTurn
		} else if !errors.Is(err, chat.ErrNotFound) {
			return err
		}

		rec := core.NewRecord(col)
		rec.Set(core.FieldNameId, turnID)
		chatSetOwner(rec, owner)
		rec.Set(chatFieldConversation, t.ConversationID)
		rec.Set(chatFieldState, string(chat.TurnStateQueued))
		rec.Set(chatFieldIdempotencyKey, t.IdempotencyKey)
		rec.Set(chatFieldIdempotencySlot, slot)
		rec.Set(chatFieldActiveSlot, t.ConversationID)
		rec.Set(chatFieldProvider, t.Provider)
		rec.Set(chatFieldModel, t.Model)
		rec.Set(chatFieldEffort, t.Effort)
		rec.Set(chatFieldLastSeq, int64(0))
		rec.Set(chatFieldDroppedBefore, int64(0))
		if err := chatSetJSON(rec, chatFieldError, nil); err != nil {
			return err
		}
		createdAt := chatTime(t.CreatedAt)
		if createdAt.IsZero() {
			createdAt = chatNow()
		}
		rec.Set(chatFieldCreatedAt, createdAt)
		rec.Set(chatFieldUpdatedAt, createdAt)
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			if chatUniqueViolation(err) {
				return errChatTurnInsertConflict
			}
			return err
		}
		out, created = chatTurnFrom(rec), true
		if err := chatTouchConversation(ctx, tx, s.conversations, owner, t.ConversationID, createdAt); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		return out, created, nil
	}
	if errors.Is(err, errChatTurnInsertConflict) || chatUniqueViolation(err) {
		// The transaction is gone (PostgreSQL aborts it on the failed insert),
		// so the winner is resolved with a fresh read.
		return s.resolveCreateTurnConflict(owner, t, slot)
	}
	return chat.Turn{}, false, err
}

// errChatTurnInsertConflict marks a CreateTurn insert that lost a unique race.
var errChatTurnInsertConflict = errors.New("chat store: turn insert lost a unique race")

// resolveCreateTurnConflict maps a lost CreateTurn insert to the contract's
// result: the same idempotency key returns the committed turn (created false),
// otherwise the conversation is now busy.
func (s *ChatStore) resolveCreateTurnConflict(owner chat.OwnerKey, t chat.Turn, slot string) (chat.Turn, bool, error) {
	params := chatOwnerParams(owner)
	params[chatFieldConversation] = t.ConversationID
	if t.IdempotencyKey != "" {
		if existing, err := chatFirst(s.app, s.turns,
			chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldIdempotencySlot+" = {:slot}",
			withParam(params, "slot", slot)); err == nil {
			return chatTurnFrom(existing), false, nil
		}
	}
	if _, err := chatFirst(s.app, s.turns,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldActiveSlot+" = {:conversation}",
		params); err == nil {
		return chat.Turn{}, false, chat.ErrActiveTurn
	}
	return chat.Turn{}, false, chat.ErrConflict
}

func (s *ChatStore) GetTurn(_ context.Context, owner chat.OwnerKey, id string) (chat.Turn, error) {
	rec, err := chatFindOwned(s.app, s.turns, owner, id)
	if err != nil {
		return chat.Turn{}, err
	}
	return chatTurnFrom(rec), nil
}

// FindTurnByIdempotencyKey implements chat.IdempotentTurnStore.
func (s *ChatStore) FindTurnByIdempotencyKey(_ context.Context, owner chat.OwnerKey, conversationID, key string) (chat.Turn, bool, error) {
	if key == "" {
		return chat.Turn{}, false, nil
	}
	params := chatOwnerParams(owner)
	params[chatFieldConversation] = conversationID
	params[chatFieldIdempotencyKey] = key
	rec, err := chatFirst(s.app, s.turns,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldConversation+" = {:conversation} && "+chatFieldIdempotencyKey+" = {:idempotencyKey}", params)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			return chat.Turn{}, false, nil
		}
		return chat.Turn{}, false, err
	}
	return chatTurnFrom(rec), true, nil
}

func (s *ChatStore) TransitionTurn(ctx context.Context, owner chat.OwnerKey, id string, from, to chat.TurnState, failure *chat.TurnFailed) (chat.Turn, error) {
	var out chat.Turn
	err := s.chatRunTx(func(tx core.App) error {
		rec, err := chatLockAndFind(tx, s.turns, owner, id)
		if err != nil {
			return err
		}
		if chat.TurnState(rec.GetString(chatFieldState)) != from {
			return chat.ErrConflict
		}
		if !chat.CanTransition(from, to) {
			return chat.ErrInvalidTransition
		}
		if to.Terminal() {
			return chat.ErrInvalidTransition
		}
		if failure != nil {
			raw, err := json.Marshal(failure)
			if err != nil {
				return fmt.Errorf("chat store: marshal turn failure: %w", err)
			}
			if err := chatSetJSON(rec, chatFieldError, raw); err != nil {
				return err
			}
		}
		rec.Set(chatFieldState, string(to))
		rec.Set(chatFieldUpdatedAt, chatNow())
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		out = chatTurnFrom(rec)
		return nil
	})
	if err != nil {
		return chat.Turn{}, err
	}
	return out, nil
}

func (s *ChatStore) FinishTurn(ctx context.Context, owner chat.OwnerKey, id string, from, to chat.TurnState, ev chat.Event) (chat.Turn, chat.Event, error) {
	type result struct {
		turn  chat.Turn
		event chat.Event
	}
	var res result
	err := s.chatRunTx(func(tx core.App) error {
		res = result{}
		rec, err := chatLockAndFind(tx, s.turns, owner, id)
		if err != nil {
			return err
		}
		state := chat.TurnState(rec.GetString(chatFieldState))
		if state != from {
			return chat.ErrConflict
		}
		if !to.Terminal() || !chat.CanTransition(from, to) {
			return chat.ErrInvalidTransition
		}
		want, ok := chatTerminalEventFor(to)
		if !ok || ev.Type != want {
			return chat.ErrInvalidTransition
		}
		if ev.TurnID != "" && ev.TurnID != id {
			return chat.ErrConflict
		}
		// A terminal state is only ever stored together with its terminal
		// event, so a non-terminal stored state implies no terminal event.
		if state.Terminal() {
			return chat.ErrConflict
		}
		failure, err := chatTerminalFailure(to, ev)
		if err != nil {
			return err
		}
		ev.TurnID = id
		if ev.ConversationID == "" {
			ev.ConversationID = rec.GetString(chatFieldConversation)
		}
		stored, err := s.appendEventTx(tx, owner, rec, ev)
		if err != nil {
			return err
		}
		rec.Set(chatFieldState, string(to))
		rec.Set(chatFieldUpdatedAt, chatNow())
		rec.Set(chatFieldActiveSlot, chatTerminalSlot(id))
		if failure != nil {
			raw, err := json.Marshal(failure)
			if err != nil {
				return fmt.Errorf("chat store: marshal turn failure: %w", err)
			}
			if err := chatSetJSON(rec, chatFieldError, raw); err != nil {
				return err
			}
		} else if err := chatSetJSON(rec, chatFieldError, nil); err != nil {
			return err
		}
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		res.turn, res.event = chatTurnFrom(rec), stored
		return nil
	})
	if err != nil {
		return chat.Turn{}, chat.Event{}, err
	}
	return res.turn, res.event, nil
}

func (s *ChatStore) AppendEvent(ctx context.Context, owner chat.OwnerKey, e chat.Event) (chat.Event, error) {
	var out chat.Event
	err := s.chatRunTx(func(tx core.App) error {
		out = chat.Event{}
		rec, err := chatLockAndFind(tx, s.turns, owner, e.TurnID)
		if err != nil {
			return err
		}
		// Terminal event types belong to FinishTurn, which stores them
		// together with the matching terminal state.
		if e.Type.Terminal() {
			return chat.ErrInvalidTransition
		}
		if chat.TurnState(rec.GetString(chatFieldState)).Terminal() {
			return chat.ErrConflict
		}
		stored, err := s.appendEventTx(tx, owner, rec, e)
		if err != nil {
			return err
		}
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		out = stored
		return nil
	})
	if err != nil {
		return chat.Event{}, err
	}
	return out, nil
}

// appendEventTx assigns the next per-turn sequence, stores the event, trims the
// retained window and updates the turn's sequence bookkeeping. The caller holds
// the turn's row lock and saves the turn inside the same transaction, so
// concurrent appends to one turn are serialized and can never assign the same
// sequence (UNIQUE(turn, seq) still guards against a writer that bypasses it).
func (s *ChatStore) appendEventTx(tx core.App, owner chat.OwnerKey, turnRec *core.Record, e chat.Event) (chat.Event, error) {
	if !e.Type.Valid() {
		return chat.Event{}, fmt.Errorf("chat: invalid event type %q: %w", string(e.Type), chat.ErrConflict)
	}
	if e.Version == "" {
		e.Version = chat.ProtocolVersion
	}
	if e.ConversationID == "" {
		e.ConversationID = turnRec.GetString(chatFieldConversation)
	}
	if e.TurnID == "" {
		e.TurnID = turnRec.Id
	}
	seq := turnRec.GetInt64(chatFieldLastSeq) + 1
	e.Seq = seq
	e.At = chatNow()

	col, err := chatCollection(tx, s.events)
	if err != nil {
		return chat.Event{}, err
	}
	rec := core.NewRecord(col)
	rec.Set(chatFieldOwnerKey, owner.String())
	rec.Set(chatFieldConversation, e.ConversationID)
	rec.Set(chatFieldTurn, e.TurnID)
	rec.Set(chatFieldSeq, seq)
	rec.Set(chatFieldType, string(e.Type))
	rec.Set(chatFieldVersion, e.Version)
	rec.Set(chatFieldAt, e.At)
	if err := chatSetJSON(rec, chatFieldData, e.Data); err != nil {
		return chat.Event{}, err
	}
	if err := tx.Save(rec); err != nil {
		return chat.Event{}, err
	}
	turnRec.Set(chatFieldLastSeq, seq)

	// Retention: keep at most maxEvents, dropping the oldest first. The
	// highest dropped sequence is recorded on the turn so a replay cursor
	// older than the window reports ErrResyncRequired instead of silently
	// skipping events.
	if cutoff := seq - int64(s.maxEvents); cutoff > turnRec.GetInt64(chatFieldDroppedBefore) {
		if err := chatDeleteEventsThrough(tx, s.events, owner, e.TurnID, cutoff); err != nil {
			return chat.Event{}, err
		}
		turnRec.Set(chatFieldDroppedBefore, cutoff)
	}
	return chatEventFrom(rec), nil
}

func chatDeleteEventsThrough(app core.App, collection string, owner chat.OwnerKey, turnID string, through int64) error {
	_, err := app.NonconcurrentDB().
		NewQuery("DELETE FROM {{" + collection + "}} WHERE [[ownerKey]] = {:ownerKey} AND [[turn]] = {:turn} AND [[seq]] <= {:seq}").
		Bind(dbx.Params{chatFieldOwnerKey: owner.String(), chatFieldTurn: turnID, chatFieldSeq: through}).
		Execute()
	if err != nil {
		return fmt.Errorf("chat store: trim %s: %w", collection, err)
	}
	return nil
}

func (s *ChatStore) EventsAfter(_ context.Context, owner chat.OwnerKey, turnID string, after int64) ([]chat.Event, error) {
	rec, err := chatFindOwned(s.app, s.turns, owner, turnID)
	if err != nil {
		return nil, err
	}
	if dropped := rec.GetInt64(chatFieldDroppedBefore); dropped > 0 && after < dropped {
		return nil, chat.ErrResyncRequired
	}
	filter := chatFieldOwnerKey + " = {:ownerKey} && " + chatFieldTurn + " = {:turn} && " + chatFieldSeq + " > {:after}"
	params := chatOwnerParams(owner)
	params[chatFieldTurn] = turnID
	params["after"] = after
	recs, err := s.app.FindRecordsByFilter(s.events, filter, chatFieldSeq, 0, 0, params)
	if err != nil {
		return nil, err
	}
	out := make([]chat.Event, 0, len(recs))
	for _, eventRec := range recs {
		out = append(out, chatEventFrom(eventRec))
	}
	return out, nil
}

// InterruptRunning ends every queued or running turn of this store's scope as
// interrupted and gives each exactly one terminal event. A store built with a
// namespace and mount (as EnableChat does) therefore never touches another
// app's turns on the same database; an unscoped store covers every turn.
func (s *ChatStore) InterruptRunning(ctx context.Context) (int, error) {
	count := 0
	err := s.chatRunTx(func(tx core.App) error {
		count = 0
		filter := chatFieldActiveSlot + " != {:empty}"
		params := dbx.Params{"empty": ""}
		if s.namespace != "" {
			filter += " && " + chatFieldNamespace + " = {:namespace} && " + chatFieldMount + " = {:mount}"
			params[chatFieldNamespace] = s.namespace
			params[chatFieldMount] = s.mount
		}
		recs, err := tx.FindRecordsByFilter(s.turns, filter, "id", 0, 0, params)
		if err != nil {
			return err
		}
		for _, candidate := range recs {
			owner := chatOwnerFrom(candidate)
			// Lock and re-read: another process in the same scope (or a
			// FinishTurn in this one) may have ended the turn after the scan.
			rec, err := chatLockAndFind(tx, s.turns, owner, candidate.Id)
			if errors.Is(err, chat.ErrNotFound) {
				continue // deleted while we scanned
			}
			if err != nil {
				return err
			}
			if chat.TurnState(rec.GetString(chatFieldState)).Terminal() {
				// Defensive: a terminal turn must never keep a conversation
				// claimed, but it already has its terminal event, so only
				// the stale slot is normalized and no work is rerun.
				want := chatTerminalSlot(rec.Id)
				if rec.GetString(chatFieldActiveSlot) != want {
					rec.Set(chatFieldActiveSlot, want)
					if err := tx.SaveWithContext(ctx, rec); err != nil {
						return err
					}
				}
				continue
			}
			if rec.GetString(chatFieldActiveSlot) == "" {
				continue // ended without a terminal state through some foreign writer
			}
			failure := chat.TurnFailed{Code: chat.FailureInterrupted, Message: chatInterruptedMessage}
			raw, err := json.Marshal(failure)
			if err != nil {
				return fmt.Errorf("chat store: marshal interrupt failure: %w", err)
			}
			ev := chat.Event{
				Version:        chat.ProtocolVersion,
				ConversationID: rec.GetString(chatFieldConversation),
				TurnID:         rec.Id,
				Type:           chat.EventTurnFailed,
				Data:           raw,
			}
			if _, err := s.appendEventTx(tx, owner, rec, ev); err != nil {
				return err
			}
			rec.Set(chatFieldState, string(chat.TurnStateInterrupted))
			rec.Set(chatFieldUpdatedAt, chatNow())
			rec.Set(chatFieldActiveSlot, chatTerminalSlot(rec.Id))
			if err := chatSetJSON(rec, chatFieldError, raw); err != nil {
				return err
			}
			if err := tx.SaveWithContext(ctx, rec); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// --- terminal state helpers (mirrors of the chat package's unexported ones) ---

func chatTerminalEventFor(s chat.TurnState) (chat.EventType, bool) {
	switch s {
	case chat.TurnStateCompleted:
		return chat.EventTurnCompleted, true
	case chat.TurnStateCancelled:
		return chat.EventTurnCancelled, true
	case chat.TurnStateFailed, chat.TurnStateInterrupted:
		return chat.EventTurnFailed, true
	}
	return "", false
}

func chatTerminalFailure(to chat.TurnState, ev chat.Event) (*chat.TurnFailed, error) {
	switch to {
	case chat.TurnStateFailed, chat.TurnStateInterrupted:
		var f chat.TurnFailed
		if len(ev.Data) == 0 {
			return nil, fmt.Errorf("chat: %s event without data: %w", ev.Type, chat.ErrConflict)
		}
		if err := json.Unmarshal(ev.Data, &f); err != nil {
			return nil, fmt.Errorf("chat: decode %s data: %w", ev.Type, chat.ErrConflict)
		}
		return &f, nil
	}
	return nil, nil
}

// --- paging cursors (same wire shape as chat.MemStore) ---

type chatCursor struct {
	at time.Time
	id string
}

func chatPageLimit(p chat.Page) int {
	switch {
	case p.Limit <= 0:
		return chatDefaultPageLimit
	case p.Limit > chatMaxPageLimit:
		return chatMaxPageLimit
	default:
		return p.Limit
	}
}

func chatEncodeCursor(at time.Time, id string) string {
	raw := strconv.FormatInt(at.UnixNano(), 10) + chatCursorSeparator + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func chatDecodeCursor(cursor string) (*chatCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("chat: invalid page cursor: %w", err)
	}
	nanos, id, ok := strings.Cut(string(raw), chatCursorSeparator)
	if !ok {
		return nil, errors.New("chat: invalid page cursor")
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("chat: invalid page cursor: %w", err)
	}
	return &chatCursor{at: time.Unix(0, n).UTC(), id: id}, nil
}

// LatestSummary implements chat.SummaryStore. The summary lives on the
// conversation record, so deleting the conversation deletes it.
func (s *ChatStore) LatestSummary(_ context.Context, owner chat.OwnerKey, conversationID string) (chat.Summary, bool, error) {
	rec, err := chatFindOwned(s.app, s.conversations, owner, conversationID)
	if err != nil {
		return chat.Summary{}, false, err
	}
	return chatSummaryFrom(rec)
}

// PutSummary implements chat.SummaryStore: it stores sum only while the
// stored summary's boundary is still prevThrough. It leaves the
// conversation's title and updatedAt alone, so a summary never reorders a
// list or looks like activity.
func (s *ChatStore) PutSummary(ctx context.Context, owner chat.OwnerKey, sum chat.Summary, prevThrough string) (bool, error) {
	raw, err := json.Marshal(sum)
	if err != nil {
		return false, err
	}
	stored := false
	err = s.chatRunTx(func(tx core.App) error {
		stored = false
		rec, err := chatLockAndFind(tx, s.conversations, owner, sum.ConversationID)
		if err != nil {
			return err
		}
		current, found, err := chatSummaryFrom(rec)
		if err != nil {
			return err
		}
		if found && current.ThroughMessageID != prevThrough || !found && prevThrough != "" {
			return nil
		}
		if err := chatSetJSON(rec, chatFieldContextSummary, raw); err != nil {
			return err
		}
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			return err
		}
		stored = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return stored, nil
}

func chatSummaryFrom(rec *core.Record) (chat.Summary, bool, error) {
	raw := chatRawJSON(rec, chatFieldContextSummary)
	if len(raw) == 0 || string(raw) == "null" {
		return chat.Summary{}, false, nil
	}
	var sum chat.Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return chat.Summary{}, false, fmt.Errorf("chat store: decode summary: %w", err)
	}
	if sum.ThroughMessageID == "" {
		return chat.Summary{}, false, nil
	}
	return sum, true, nil
}

// ImportConversation implements chat.ImportStore: the conversation and its
// messages are written in one transaction, and an owner's second import of a
// SourceID returns the first conversation without writing anything.
func (s *ChatStore) ImportConversation(ctx context.Context, owner chat.OwnerKey, c chat.ImportedConversation) (chat.Conversation, bool, error) {
	if err := owner.Validate(); err != nil {
		return chat.Conversation{}, false, err
	}
	// The "already imported?" check and the insert must not interleave: two
	// tabs (or a retry after a slow answer) import the same thread at once.
	// Imports are rare, so one lock per store is enough. It covers one
	// process; instances sharing a database rely on imports being one-off.
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if err := chat.ValidateImport(&c, time.Now()); err != nil {
		return chat.Conversation{}, false, err
	}
	// A message must fit this store's parts column; say which one doesn't
	// (a 400), rather than failing the transaction (a 500).
	for i, m := range c.Messages {
		raw, err := json.Marshal(m.Parts)
		if err != nil {
			return chat.Conversation{}, false, err
		}
		if limit := chatMaxJSONBytes; len(raw) > limit {
			return chat.Conversation{}, false, fmt.Errorf("%w: message %d is %d bytes, over this store's %d-byte limit per message",
				chat.ErrInvalidImport, i, len(raw), limit)
		}
	}
	title := c.Title
	if title == "" {
		title = chat.ImportTitle(c)
	}
	var (
		out     chat.Conversation
		created bool
	)
	err := s.chatRunTx(func(tx core.App) error {
		created = false
		params := chatOwnerParams(owner)
		params[chatFieldImportSource] = c.SourceID
		if existing, err := chatFirst(tx, s.conversations,
			chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldImportSource+" = {:importSource}", params); err == nil {
			out = chatConversationFrom(existing)
			return nil
		} else if !errors.Is(err, chat.ErrNotFound) {
			return err
		}
		convCol, err := chatCollection(tx, s.conversations)
		if err != nil {
			return err
		}
		conv := core.NewRecord(convCol)
		chatSetOwner(conv, owner)
		conv.Set(chatFieldTitle, title)
		conv.Set(chatFieldTarget, c.Target)
		conv.Set(chatFieldCreatedAt, chatTime(c.CreatedAt))
		conv.Set(chatFieldUpdatedAt, chatTime(c.UpdatedAt))
		conv.Set(chatFieldImportSource, c.SourceID)
		if err := tx.SaveWithContext(ctx, conv); err != nil {
			return err
		}
		msgCol, err := chatCollection(tx, s.messages)
		if err != nil {
			return err
		}
		for _, m := range c.Messages {
			rec := core.NewRecord(msgCol)
			chatSetOwner(rec, owner)
			rec.Set(chatFieldConversation, conv.Id)
			rec.Set(chatFieldTurn, "")
			rec.Set(chatFieldRole, m.Role)
			raw, err := json.Marshal(m.Parts)
			if err != nil {
				return fmt.Errorf("chat store: marshal imported parts: %w", err)
			}
			if err := chatSetJSON(rec, chatFieldParts, raw); err != nil {
				return err
			}
			rec.Set(chatFieldCreatedAt, chatTime(m.CreatedAt))
			if err := tx.SaveWithContext(ctx, rec); err != nil {
				return err
			}
		}
		out = chatConversationFrom(conv)
		created = true
		return nil
	})
	if err != nil {
		return chat.Conversation{}, false, err
	}
	return out, created, nil
}
