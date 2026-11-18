package chat_test

// The behavioural Store contract now lives in chat/storetest and is run here
// against the reference in-memory implementation. What remains below is
// MemStore-specific: the shape of its generated ids, which no other adapter
// has to reproduce.

import (
	"context"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/storetest"
)

func background() context.Context { return context.Background() }

func TestMemStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) chat.Store {
		return chat.NewMemStore(chat.MemStoreOptions{})
	})
}

func TestMemStoreRetention(t *testing.T) {
	storetest.RunRetention(t, func(t *testing.T, max int) chat.Store {
		return chat.NewMemStore(chat.MemStoreOptions{MaxEventsPerTurn: max})
	})
}

func TestMemStoreProposals(t *testing.T) {
	storetest.RunProposals(t, func(t *testing.T) interface {
		chat.Store
		chat.ProposalStore
	} {
		return chat.NewMemStore(chat.MemStoreOptions{})
	})
}

func TestMemStoreUploads(t *testing.T) {
	storetest.RunUploads(t, func(t *testing.T) interface {
		chat.Store
		chat.UploadStore
	} {
		return chat.NewMemStore(chat.MemStoreOptions{})
	})
}

// TestMemStoreIDsAreRandom is MemStore-internal: the length and alphabet of
// its identifiers are an implementation detail, not part of the Store
// contract (a database adapter lets its database assign ids).
func TestMemStoreIDsAreRandom(t *testing.T) {
	const idLength = 15
	s := chat.NewMemStore(chat.MemStoreOptions{})
	owner := chat.OwnerKey{
		Namespace:        "app",
		Mount:            "/mcp",
		AuthCollectionID: "users0000000001",
		UserID:           "user00000000001",
	}
	seen := make(map[string]bool, 50)
	for i := 0; i < 50; i++ {
		c, err := s.CreateConversation(background(), chat.Conversation{Owner: owner, Title: "chat"})
		if err != nil {
			t.Fatal(err)
		}
		if len(c.ID) != idLength {
			t.Fatalf("id %q has length %d, want %d", c.ID, len(c.ID), idLength)
		}
		for _, r := range c.ID {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z') {
				t.Fatalf("id %q is not lowercase alphanumeric", c.ID)
			}
		}
		if seen[c.ID] {
			t.Fatalf("duplicate id %q", c.ID)
		}
		seen[c.ID] = true
	}
}
