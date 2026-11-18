package chat

import "context"

// UploadStore is the persistence port for chat attachments. It is deliberately
// separate from Store: upload support is additive, so an adapter that only
// implements Store keeps compiling and the upload surface stays switched off
// for it.
//
// Every method takes the caller's full OwnerKey and compares it; an upload
// owned by someone else is reported exactly like a missing one. Each method also
// re-checks that the upload's conversation still belongs to that owner, so a
// deleted or foreign thread can never be read through an upload id.
//
// Attachments are the chat service's own records. Deleting a conversation
// deletes that conversation's uploads through Store.DeleteConversation; it
// never touches an application's original asset.
//
// Implementations must be safe for concurrent use and copy the data they are
// given and return, so no caller can mutate stored bytes.
type UploadStore interface {
	// CreateUpload stores one attachment. The store assigns ID, CreatedAt,
	// Size (len(data)) and SHA256 ("sha256:" + lowercase hex of the data's
	// SHA-256) and ignores those fields on input. ConversationID is required
	// and must be the owner's conversation, else ErrNotFound. Name, MIME and
	// Kind are the caller's (already validated) values. A duplicate ID returns
	// ErrConflict.
	CreateUpload(ctx context.Context, owner OwnerKey, u Upload, data []byte) (Upload, error)

	// GetUpload returns one caller-owned upload without its bytes.
	GetUpload(ctx context.Context, owner OwnerKey, id string) (Upload, error)

	// ReadUpload returns one caller-owned upload and a copy of its bytes.
	ReadUpload(ctx context.Context, owner OwnerKey, id string) (Upload, []byte, error)

	// ListUploads returns one conversation's uploads, newest first by
	// (CreatedAt, ID). nextCursor is empty when the page is the last one. It
	// returns ErrNotFound when the conversation is not the caller's.
	ListUploads(ctx context.Context, owner OwnerKey, conversationID string, page Page) ([]Upload, string, error)

	// CountUploads returns how many uploads the conversation holds, for the
	// per-conversation limit. It returns ErrNotFound when the conversation is
	// not the caller's.
	CountUploads(ctx context.Context, owner OwnerKey, conversationID string) (int, error)
}

// uploadSHA256Prefix is the algorithm marker stored with every upload hash. It
// is part of the port's contract, not an implementation detail, so a client can
// tell which digest it holds.
const uploadSHA256Prefix = "sha256:"
