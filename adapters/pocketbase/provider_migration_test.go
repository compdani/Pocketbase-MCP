package pocketbase

import (
	"context"
	"testing"

	"github.com/compdani/pocketbase-mcp/chat"
)

func TestChatStoreAddsProviderFieldOnUpgrade(t *testing.T) {
	app := newChatTestApp(t)
	store, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owner := chat.OwnerKey{Namespace: "ns", Mount: "/mcp", AuthCollectionID: "users", UserID: "u1"}
	conv, err := store.CreateConversation(context.Background(), chat.Conversation{Owner: owner, Title: "c"})
	if err != nil {
		t.Fatal(err)
	}
	turn, created, err := store.CreateTurn(context.Background(), owner, chat.Turn{
		ConversationID: conv.ID,
		Provider:       "openai",
		Model:          "m1",
	})
	if err != nil || !created || turn.Provider != "openai" {
		t.Fatalf("turn = %+v created = %v err = %v", turn, created, err)
	}

	// Simulate a v1.2 store: the provider column did not exist yet.
	col, err := app.FindCollectionByNameOrId(store.turns)
	if err != nil {
		t.Fatal(err)
	}
	col.Fields.RemoveByName(chatFieldProvider)
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	if col.Fields.GetByName(chatFieldProvider) != nil {
		t.Fatal("provider field was not removed for the fixture")
	}

	upgraded, err := NewChatStore(app, ChatStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	col, err = app.FindCollectionByNameOrId(upgraded.turns)
	if err != nil {
		t.Fatal(err)
	}
	if col.Fields.GetByName(chatFieldProvider) == nil {
		t.Fatal("provider field was not added on upgrade")
	}
	stored, err := upgraded.GetTurn(context.Background(), owner, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Model != "m1" || stored.Provider != "" {
		t.Fatalf("stored turn = %+v", stored)
	}
}
