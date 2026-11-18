package main

import (
	"encoding/json"
	"strings"
	"testing"

	pbmcp "github.com/compdani/pocketbase-mcp"
	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// demoRequest builds a request whose last message is a user message with text.
func demoRequest(text string, tools ...provider.ToolDef) provider.Request {
	return provider.Request{
		Model:    "demo-1",
		Tools:    tools,
		Messages: []provider.Message{{Role: provider.RoleUser, Text: text}},
	}
}

func replyText(chunks []provider.Chunk) string {
	var b strings.Builder
	for _, chunk := range chunks {
		if chunk.Kind == provider.ChunkText {
			b.WriteString(chunk.Text)
		}
	}
	return b.String()
}

func onlyToolCall(t *testing.T, chunks []provider.Chunk) *provider.ToolCall {
	t.Helper()
	var call *provider.ToolCall
	for i := range chunks {
		if chunks[i].Kind == provider.ChunkToolCall {
			if call != nil {
				t.Fatalf("more than one tool call chunk")
			}
			call = chunks[i].ToolCall
		}
	}
	if call == nil {
		t.Fatalf("no tool call chunk in %#v", chunks)
	}
	return call
}

func decodeArgs(t *testing.T, call *provider.ToolCall) map[string]any {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("arguments are not a JSON object: %v (%s)", err, call.Arguments)
	}
	return args
}

func createSchema(collectionEnum []any) map[string]any {
	collection := map[string]any{"type": "string"}
	if collectionEnum != nil {
		collection["enum"] = collectionEnum
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"collection": collection,
			"data":       map[string]any{"type": "object", "additionalProperties": true},
		},
		"required": []string{"collection", "data"},
	}
}

func TestDemoProviderSupportsEveryAttachmentType(t *testing.T) {
	p := &demoProvider{}
	for _, mimeType := range provider.AttachmentMIMETypes() {
		if !p.SupportsAttachment(mimeType) {
			t.Fatalf("demo provider must support %s", mimeType)
		}
	}
	if p.SupportsAttachment("application/zip") {
		t.Fatalf("demo provider must not support an unaccepted type")
	}
}

func TestDemoAttachmentReplyNamesFilesAndCountsTextCharacters(t *testing.T) {
	req := provider.Request{
		Model: "demo-1",
		Messages: []provider.Message{{
			Role: provider.RoleUser,
			Attachments: []provider.Attachment{
				{Name: "notes.txt", MIME: "text/plain", Data: []byte("héllo")},
				{Name: "shot.png", MIME: "image/png", Data: []byte("\x89PNG\r\n\x1a\n")},
			},
		}},
	}

	text := replyText(demoChunks(req))
	if !strings.Contains(text, "I received 2 attachment(s): notes.txt, shot.png.") {
		t.Fatalf("reply does not name both attachments: %q", text)
	}
	if !strings.Contains(text, "The text attachment(s) notes.txt contain 5 character(s).") {
		t.Fatalf("reply does not count text characters (runes): %q", text)
	}
}

func TestDemoCreateCallUsesPerCollectionTool(t *testing.T) {
	req := demoRequest("please create Buy milk",
		provider.ToolDef{Name: "search_items"},
		provider.ToolDef{Name: "create_items", InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"data": map[string]any{"type": "object"},
			},
			"required": []string{"data"},
		}},
	)

	call := onlyToolCall(t, demoChunks(req))
	if call.Name != "create_items" {
		t.Fatalf("tool = %q, want create_items", call.Name)
	}
	args := decodeArgs(t, call)
	data, ok := args["data"].(map[string]any)
	if !ok {
		t.Fatalf("arguments have no data object: %#v", args)
	}
	if data["title"] != "Buy milk" {
		t.Fatalf("title = %#v, want Buy milk", data["title"])
	}
	if _, ok := args["collection"]; ok {
		t.Fatalf("a per-collection call must not carry a collection: %#v", args)
	}
}

func TestDemoCreateCallUsesGenericToolAndFirstCollection(t *testing.T) {
	req := demoRequest("create",
		provider.ToolDef{Name: "create", InputSchema: createSchema([]any{"items", "reports"})},
	)

	call := onlyToolCall(t, demoChunks(req))
	if call.Name != "create" {
		t.Fatalf("tool = %q, want create", call.Name)
	}
	args := decodeArgs(t, call)
	if args["collection"] != "items" {
		t.Fatalf("collection = %#v, want the first enum value", args["collection"])
	}
	data, _ := args["data"].(map[string]any)
	if data["title"] != "Demo item" {
		t.Fatalf("title = %#v, want the canned title", data["title"])
	}
}

func TestDemoEditCallUsesTypedRevisionAndText(t *testing.T) {
	req := demoRequest("edit rev:3 Hello there", provider.ToolDef{Name: "edit_note"})

	call := onlyToolCall(t, demoChunks(req))
	if call.Name != "edit_note" {
		t.Fatalf("tool = %q, want edit_note", call.Name)
	}
	args := decodeArgs(t, call)
	if args["editorId"] != "demo-note" {
		t.Fatalf("editorId = %#v", args["editorId"])
	}
	if args["baseRevision"] != "3" {
		t.Fatalf("baseRevision = %#v, want 3", args["baseRevision"])
	}
	if args["text"] != "Hello there" {
		t.Fatalf("text = %#v, want Hello there", args["text"])
	}

	canned := onlyToolCall(t, demoChunks(demoRequest("edit", provider.ToolDef{Name: "edit_note"})))
	cannedArgs := decodeArgs(t, canned)
	if cannedArgs["baseRevision"] != "0" {
		t.Fatalf("baseRevision = %#v, want 0", cannedArgs["baseRevision"])
	}
	if cannedArgs["text"] != "Edited by the demo assistant" {
		t.Fatalf("text = %#v, want the canned text", cannedArgs["text"])
	}
}

func TestDemoCreateWithoutAToolExplainsReviewIsOff(t *testing.T) {
	text := replyText(demoChunks(demoRequest("create Buy milk")))
	if !strings.Contains(text, "MCP_CHAT_PROPOSALS=true") {
		t.Fatalf("reply should name the review setting: %q", text)
	}
	if strings.Contains(strings.ToLower(text), "created") {
		t.Fatalf("reply must not claim a write: %q", text)
	}
}

func TestDemoToolSummaryReportsPendingApproval(t *testing.T) {
	text := replyText(demoChunks(provider.Request{
		Model: "demo-1",
		Messages: []provider.Message{{
			Role:       provider.RoleTool,
			ToolName:   "create_items",
			ToolCallID: "demo-create-1",
			ToolResult: json.RawMessage(`{"status":"pending_review","proposalId":"p1","summary":"Create an items record"}`),
		}},
	}))

	if !strings.Contains(text, "waiting for your approval") {
		t.Fatalf("reply does not say the change is pending: %q", text)
	}
	if !strings.Contains(text, "Nothing has been applied") {
		t.Fatalf("reply does not deny applying the change: %q", text)
	}
}

func TestDemoEditNoteToolPassesStartupValidation(t *testing.T) {
	cfg := pbmcp.Config{CustomTools: []pbmcp.ToolSpec{demoEditNoteTool()}, AllowWrites: false}
	if err := cfg.ValidateCustomTools(); err != nil {
		t.Fatalf("edit_note must pass the editor tool contract: %v", err)
	}
	tool := demoEditNoteTool()
	if tool.Channels != "chat" || tool.ExecutionLocation != "editor" || tool.Handler != nil {
		t.Fatalf("edit_note must be a chat-only editor tool without a handler: %#v", tool)
	}
}
