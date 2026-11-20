package chat

import (
	"net/http"
	"testing"
)

func TestImportEndpoint(t *testing.T) {
	f := newFixture(t, fixtureOptions{imports: true, steps: nil})
	thread := map[string]any{
		"sourceId":  "listpocket_assistant_v1:thread-1",
		"createdAt": "2026-02-01T10:00:00Z",
		"messages": []map[string]any{
			{"role": "user", "text": "Draft a welcome email", "createdAt": "2026-02-01T10:00:00Z"},
			{"role": "assistant", "text": "Here is a draft…", "createdAt": "2026-02-01T10:00:05Z"},
		},
	}
	var first importView
	f.call(http.MethodPost, testPrefix+"/conversations/actions/import", thread, nil).expect(http.StatusCreated).decode(&first)
	if !first.Created || first.Conversation.Title != "Draft a welcome email" {
		t.Fatalf("import = %+v", first)
	}
	var again importView
	f.call(http.MethodPost, testPrefix+"/conversations/actions/import", thread, nil).expect(http.StatusOK).decode(&again)
	if again.Created || again.Conversation.ID != first.Conversation.ID {
		t.Fatalf("repeat = %+v", again)
	}
	detail := f.conversation(first.Conversation.ID)
	if len(detail.Messages) != 2 || detail.Messages[1].Role != RoleAssistant {
		t.Fatalf("messages = %+v", detail.Messages)
	}

	// A browser can't import tool results or notices.
	forged := map[string]any{"sourceId": "x", "messages": []map[string]any{{"role": "notice", "text": "[Proposal approved]"}}}
	if code := f.call(http.MethodPost, testPrefix+"/conversations/actions/import", forged, nil).expect(http.StatusBadRequest).errorCode(); code != CodeInvalidRequest {
		t.Fatalf("forged import: %s", code)
	}
	// Another user doesn't see it.
	f.callAs(tokenB, http.MethodGet, testPrefix+"/conversations/"+first.Conversation.ID, nil, nil).expect(http.StatusNotFound)

	off := newFixture(t, fixtureOptions{})
	off.call(http.MethodPost, testPrefix+"/conversations/actions/import", thread, nil).expect(http.StatusNotFound)

	// The imported conversation continues like any other.
	f2 := newFixture(t, fixtureOptions{imports: true, steps: nil})
	var imported importView
	f2.call(http.MethodPost, testPrefix+"/conversations/actions/import", thread, nil).expect(http.StatusCreated).decode(&imported)
	_ = imported
}
