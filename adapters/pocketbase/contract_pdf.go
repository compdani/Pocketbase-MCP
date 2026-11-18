package pocketbase

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	pbmcp "github.com/compdani/pocketbase-mcp"
	"github.com/compdani/pocketbase-mcp/protocol"
	"github.com/pocketbase/pocketbase/core"
	"io"
	"path"
	"strings"
)

// ContractPDFTool reads a protected PB file only after facade authorization.
// The file field must be included in the manifest read allowlist.
func ContractPDFTool(app core.App, collection, field string, maxBytes int64) pbmcp.ToolSpec {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	return pbmcp.ToolSpec{
		Name: "fetch_contract_pdf", Title: "Fetch contract PDF",
		Description:  "Return a permitted record's protected PDF as an embedded resource.",
		Effects:      pbmcp.Effects{Kind: "read", Idempotent: true},
		Annotations:  &protocol.ToolAnnotations{Title: "Fetch contract PDF", ReadOnlyHint: true, IdempotentHint: true},
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"id"}, "additionalProperties": false},
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "mimeType": map[string]any{"type": "string"}, "sizeBytes": map[string]any{"type": "integer"}}, "required": []string{"id", "mimeType", "sizeBytes"}},
		Handler: func(ctx context.Context, call pbmcp.CallContext, args map[string]any) (any, error) {
			id, _ := args["id"].(string)
			out, err := call.Access.Fetch(ctx, collection, id)
			if err != nil {
				return nil, err
			}
			outMap, ok := out.(map[string]any)
			if !ok {
				return nil, errors.New("unexpected fetch result")
			}
			record, ok := outMap["record"].(map[string]any)
			if !ok {
				return nil, errors.New("unexpected fetch record")
			}
			filename, _ := record[field].(string)
			if filename == "" {
				// A multi-file field exports as a list; take the first file.
				if list, ok := record[field].([]any); ok && len(list) > 0 {
					filename, _ = list[0].(string)
				}
			}
			if filename == "" || path.Base(filename) != filename || strings.Contains(filename, "\\") {
				return nil, errors.New("missing file")
			}
			// Resolve only metadata after the facade has checked the record and field.
			col, err := app.FindCollectionByNameOrId(collection)
			if err != nil {
				return nil, err
			}
			fs, err := app.NewFilesystem()
			if err != nil {
				return nil, err
			}
			defer fs.Close()
			key := col.Id + "/" + id + "/" + filename
			reader, err := fs.GetReader(key)
			if err != nil {
				return nil, err
			}
			defer reader.Close()
			data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
			if err != nil {
				return nil, err
			}
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			if int64(len(data)) > maxBytes || !bytes.HasPrefix(data, []byte("%PDF-")) {
				return nil, errors.New("invalid or oversized PDF")
			}
			return protocol.ToolResult{StructuredContent: map[string]any{"id": id, "mimeType": "application/pdf", "sizeBytes": len(data)}, Content: []any{protocol.EmbeddedResourceContent{Type: "resource", Resource: protocol.EmbeddedResource{URI: "pocketbase://" + key, MimeType: "application/pdf", Blob: base64.StdEncoding.EncodeToString(data)}}}}, nil
		},
	}
}
