package pbpostgress

// Uploads: the chat.UploadStore half of ChatStore, on the fork.
//
// An upload row is metadata plus one protected file field. The bytes are
// written through filesystem.NewFileFromBytes into the app's configured
// filesystem (local or S3), and ReadUpload streams them back through the chat
// HTTP layer; the store never forms a public URL or a file token for them.
//
// The fork runs several app instances against one database, so the two
// read-then-write decisions take the same row lock the rest of the store uses:
// CreateUpload locks the conversation before it inserts, which serializes
// uploads with DeleteConversation and with each other on that conversation.
//
// Ownership is enforced by every query: another owner's upload is
// indistinguishable from a missing one, and an upload whose conversation is gone
// is reported as missing. Deleting a conversation deletes its upload records in
// the same transaction, and the app deletes each record's file as part of the
// record delete.
//
// The per-conversation count limit is enforced by the HTTP layer, which calls
// CountUploads and then CreateUpload; the port has no limit parameter, so the two
// calls are not one critical section. The conversation lock keeps concurrent
// inserts into one conversation serialized, but a caller could still race the
// count between them, so the limit is best-effort with a bounded overshoot. Every
// attachment is still owned, type-checked and delivered only to its owner.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"

	"github.com/compdani/pocketbase-mcp/chat"
)

const (
	// chatMaxUploadFileBytes is the storage-level bound of one attachment.
	// The HTTP layer enforces the deployment's real limit before the store is
	// called; this only keeps a direct store caller from writing an unbounded
	// file.
	chatMaxUploadFileBytes int64 = 64 << 20
	// chatUploadSHA256Prefix is the algorithm marker stored with every digest.
	// It is part of the chat.UploadStore contract.
	chatUploadSHA256Prefix = "sha256:"
)

// chatUploadFrom maps one stored record to the port's value. The bytes are
// deliberately not part of it; only ReadUpload returns them.
func chatUploadFrom(rec *core.Record) chat.Upload {
	return chat.Upload{
		ID:             rec.Id,
		ConversationID: rec.GetString(chatFieldUploadConversation),
		Owner:          chatOwnerFrom(rec),
		Name:           rec.GetString(chatFieldUploadName),
		MIME:           rec.GetString(chatFieldUploadMIME),
		Kind:           rec.GetString(chatFieldUploadKind),
		Size:           rec.GetInt64(chatFieldUploadSize),
		SHA256:         rec.GetString(chatFieldUploadSHA256),
		CreatedAt:      rec.GetDateTime(chatFieldCreatedAt).Time(),
	}
}

// chatUploadTime returns a strictly increasing stored-date timestamp for one
// upload. The stored date keeps millisecond precision, so two uploads stored in
// the same millisecond would tie; the port's newest-first order is by
// (CreatedAt, ID), and creation order is the useful one for a transcript. The
// clamp is per store instance, so a tie across instances still resolves by id.
func (s *ChatStore) chatUploadTime() time.Time {
	for {
		last := s.lastUploadMS.Load()
		now := chatNow().UnixMilli()
		if now <= last {
			now = last + 1
		}
		if s.lastUploadMS.CompareAndSwap(last, now) {
			return time.UnixMilli(now).UTC()
		}
	}
}

// chatOwnedUpload resolves an upload only when it belongs to owner and its
// conversation still does too. A record that survived its conversation, or whose
// owner differs, is reported as missing.
func (s *ChatStore) chatOwnedUpload(owner chat.OwnerKey, id string) (*core.Record, error) {
	rec, err := chatFindOwned(s.app, s.uploads, owner, id)
	if err != nil {
		return nil, err
	}
	if _, err := chatFindOwned(s.app, s.conversations, owner, rec.GetString(chatFieldUploadConversation)); err != nil {
		return nil, err
	}
	return rec, nil
}

// CreateUpload stores one attachment. The conversation must be the caller's,
// else the upload is reported as missing. The store assigns the id, timestamp,
// size and SHA-256 and ignores those fields on input.
func (s *ChatStore) CreateUpload(ctx context.Context, owner chat.OwnerKey, u chat.Upload, data []byte) (chat.Upload, error) {
	if err := owner.Validate(); err != nil {
		return chat.Upload{}, err
	}
	if int64(len(data)) > chatMaxUploadFileBytes {
		return chat.Upload{}, fmt.Errorf("chat store: upload is %d bytes, the storage limit is %d", len(data), chatMaxUploadFileBytes)
	}
	// The stored file name is randomized by the app, so the caller's name only
	// has to be a safe base name for the file layer. The metadata row keeps the
	// caller's (already validated) name.
	file, err := filesystem.NewFileFromBytes(data, chat.SanitizeUploadName(u.Name))
	if err != nil {
		return chat.Upload{}, fmt.Errorf("chat store: build upload file: %w", err)
	}
	sum := sha256.Sum256(data)

	var out chat.Upload
	err = s.chatRunTx(func(tx core.App) error {
		out = chat.Upload{}
		// Locking the conversation serializes this insert with a delete and
		// with other uploads into the same conversation.
		if err := s.chatLockConversation(tx, owner, u.ConversationID); err != nil {
			return err
		}
		col, err := chatCollection(tx, s.uploads)
		if err != nil {
			return err
		}
		rec := core.NewRecord(col)
		id := u.ID
		if id == "" {
			id = core.GenerateDefaultRandomId()
		}
		rec.Set(core.FieldNameId, id)
		chatSetOwner(rec, owner)
		rec.Set(chatFieldUploadConversation, u.ConversationID)
		rec.Set(chatFieldUploadName, u.Name)
		rec.Set(chatFieldUploadMIME, u.MIME)
		rec.Set(chatFieldUploadKind, u.Kind)
		rec.Set(chatFieldUploadSize, int64(len(data)))
		rec.Set(chatFieldUploadSHA256, chatUploadSHA256Prefix+hex.EncodeToString(sum[:]))
		rec.Set(chatFieldUploadData, file)
		ts := s.chatUploadTime()
		rec.Set(chatFieldCreatedAt, ts)
		rec.Set(chatFieldUpdatedAt, ts)
		if err := tx.SaveWithContext(ctx, rec); err != nil {
			if chatUniqueViolation(err) {
				return chat.ErrConflict
			}
			return err
		}
		out = chatUploadFrom(rec)
		return nil
	})
	if err != nil {
		return chat.Upload{}, err
	}
	return out, nil
}

// GetUpload returns one caller-owned upload's metadata, without its bytes.
func (s *ChatStore) GetUpload(_ context.Context, owner chat.OwnerKey, id string) (chat.Upload, error) {
	rec, err := s.chatOwnedUpload(owner, id)
	if err != nil {
		return chat.Upload{}, err
	}
	return chatUploadFrom(rec), nil
}

// ReadUpload returns one caller-owned upload and a copy of its bytes. The stored
// size bounds the read, so a corrupted or truncated file can never make the
// reader hand back more than the metadata claims.
func (s *ChatStore) ReadUpload(ctx context.Context, owner chat.OwnerKey, id string) (chat.Upload, []byte, error) {
	rec, err := s.chatOwnedUpload(owner, id)
	if err != nil {
		return chat.Upload{}, nil, err
	}
	upload := chatUploadFrom(rec)
	filename := rec.GetString(chatFieldUploadData)
	if filename == "" {
		return chat.Upload{}, nil, fmt.Errorf("chat store: upload %q has no stored file: %w", id, chat.ErrNotFound)
	}
	fsys, err := s.app.NewFilesystem()
	if err != nil {
		return chat.Upload{}, nil, fmt.Errorf("chat store: open upload storage: %w", err)
	}
	defer fsys.Close()
	fsys.SetContext(ctx)
	reader, err := fsys.GetReader(rec.BaseFilesPath() + "/" + filename)
	if err != nil {
		return chat.Upload{}, nil, fmt.Errorf("chat store: read upload %q: %w", id, err)
	}
	defer reader.Close()

	data, err := io.ReadAll(io.LimitReader(reader, upload.Size+1))
	if err != nil {
		return chat.Upload{}, nil, fmt.Errorf("chat store: read upload %q: %w", id, err)
	}
	if int64(len(data)) != upload.Size {
		return chat.Upload{}, nil, fmt.Errorf("chat store: upload %q is %d bytes, expected %d", id, len(data), upload.Size)
	}
	return upload, data, nil
}

// ListUploads returns one conversation's uploads, newest first by
// (CreatedAt, ID).
func (s *ChatStore) ListUploads(_ context.Context, owner chat.OwnerKey, conversationID string, page chat.Page) ([]chat.Upload, string, error) {
	if _, err := chatFindOwned(s.app, s.conversations, owner, conversationID); err != nil {
		return nil, "", err
	}
	limit := chatPageLimit(page)
	after, err := chatDecodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter := chatFieldOwnerKey + " = {:ownerKey} && " + chatFieldUploadConversation + " = {:conversation}"
	params := chatOwnerParams(owner)
	params["conversation"] = conversationID
	if after != nil {
		filter += " && (" + chatFieldCreatedAt + " < {:at} || (" + chatFieldCreatedAt + " = {:at} && id < {:id}))"
		params["at"] = chatDateParam(after.at)
		params["id"] = after.id
	}
	recs, err := s.app.FindRecordsByFilter(s.uploads, filter, "-"+chatFieldCreatedAt+",-id", limit+1, 0, params)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(recs) > limit {
		recs = recs[:limit]
		last := recs[limit-1]
		next = chatEncodeCursor(last.GetDateTime(chatFieldCreatedAt).Time(), last.Id)
	}
	out := make([]chat.Upload, 0, len(recs))
	for _, rec := range recs {
		out = append(out, chatUploadFrom(rec))
	}
	return out, next, nil
}

// CountUploads returns how many uploads one caller-owned conversation holds.
func (s *ChatStore) CountUploads(_ context.Context, owner chat.OwnerKey, conversationID string) (int, error) {
	if _, err := chatFindOwned(s.app, s.conversations, owner, conversationID); err != nil {
		return 0, err
	}
	params := chatOwnerParams(owner)
	params["conversation"] = conversationID
	recs, err := s.app.FindRecordsByFilter(s.uploads,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldUploadConversation+" = {:conversation}",
		"", 0, 0, params)
	if err != nil {
		return 0, err
	}
	return len(recs), nil
}

// chatDeleteConversationUploads removes one conversation's uploads of one owner
// inside the caller's transaction. Each record is deleted through the app so its
// file is removed from the configured filesystem; a bulk DELETE would leave the
// bytes behind. The collection and column names come from the provisioned
// schema, never from user input, and every value is bound.
func chatDeleteConversationUploads(ctx context.Context, app core.App, collection string, owner chat.OwnerKey, conversationID string) error {
	params := chatOwnerParams(owner)
	params["conversation"] = conversationID
	recs, err := app.FindRecordsByFilter(collection,
		chatFieldOwnerKey+" = {:ownerKey} && "+chatFieldUploadConversation+" = {:conversation}",
		"", 0, 0, params)
	if err != nil {
		return fmt.Errorf("chat store: list %s for delete: %w", collection, err)
	}
	for _, rec := range recs {
		if err := app.DeleteWithContext(ctx, rec); err != nil {
			return fmt.Errorf("chat store: delete upload %q: %w", rec.Id, err)
		}
	}
	return nil
}
