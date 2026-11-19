package pocketbase

import (
	"context"
	"net/http"
	"testing"

	pbmcp "github.com/compdani/pocketbase-mcp"
	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// titlingProvider answers title requests with a fixed title and everything
// else with a short reply.
type titlingProvider struct{}

func (titlingProvider) Name() string { return "titling" }

func (titlingProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	text := "A reply."
	if req.System == chat.TitleSystemPrompt {
		text = "Contracts closing soon"
	}
	ch := make(chan provider.Chunk, 2)
	ch <- provider.Chunk{Kind: provider.ChunkText, Text: text}
	ch <- provider.Chunk{Kind: provider.ChunkDone}
	close(ch)
	return ch, nil
}

func TestChatTitleModelTitlesAndTouchesTheConversation(t *testing.T) {
	env := newChatEnv(t, chatEnvOptions{enable: func(srv *Server) error {
		return srv.EnableChat(ChatConfig{
			Options:    pbmcp.ChatOptions{Namespace: "chat-tests", Prefix: chatTestPrefix},
			Provider:   titlingProvider{},
			Model:      "m",
			TitleModel: "auto",
		})
	}})
	conv, _ := env.runTurn("Which accepted contracts close in the next 14 days?")

	var list struct {
		Items []chat.Conversation `json:"items"`
	}
	if code := env.json(env.userToken, http.MethodGet, chatTestPrefix+"/conversations", "", &list); code != http.StatusOK {
		t.Fatalf("list: HTTP %d", code)
	}
	if len(list.Items) != 1 || list.Items[0].ID != conv.ID {
		t.Fatalf("list = %+v", list.Items)
	}
	got := list.Items[0]
	if got.Title != "Contracts closing soon" {
		t.Fatalf("title = %q, want the generated title by the time the turn ended", got.Title)
	}
	if !got.UpdatedAt.After(conv.CreatedAt) {
		t.Fatalf("updatedAt %v did not move past createdAt %v", got.UpdatedAt, conv.CreatedAt)
	}
}

func TestChatTitlesAreOffByDefault(t *testing.T) {
	env := newChatEnv(t, chatEnvOptions{enable: func(srv *Server) error {
		return srv.EnableChat(ChatConfig{
			Options:  pbmcp.ChatOptions{Namespace: "chat-tests", Prefix: chatTestPrefix},
			Provider: titlingProvider{},
			Model:    "m",
		})
	}})
	conv, _ := env.runTurn("hello")
	var got chat.Conversation
	var detail struct {
		Conversation chat.Conversation `json:"conversation"`
	}
	if code := env.json(env.userToken, http.MethodGet, chatTestPrefix+"/conversations/"+conv.ID, "", &detail); code != http.StatusOK {
		t.Fatalf("get: HTTP %d", code)
	}
	got = detail.Conversation
	if got.Title != "" {
		t.Fatalf("title = %q, want none without TitleModel", got.Title)
	}
}
