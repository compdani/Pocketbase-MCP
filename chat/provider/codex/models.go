package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Model is one model offered to the signed-in plan account.
type Model struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	// Efforts are the reasoning efforts the plan offers for the model
	// (supported_reasoning_levels), in the order the endpoint lists them.
	Efforts []string `json:"efforts,omitempty"`
}

// ListModels returns the models the account may use. The plan endpoint returns
// both data[] and models[] shapes; entries whose visibility is set and is not
// "list" are hidden from the picker.
func ListModels(ctx context.Context, hc *http.Client, base, token string) ([]Model, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	var body struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	list := append(body.Data, body.Models...)
	out := make([]Model, 0, len(list))
	for _, m := range list {
		if v, ok := m["visibility"].(string); ok && v != "list" {
			continue
		}
		slug, _ := m["slug"].(string)
		if slug == "" {
			slug, _ = m["id"].(string)
		}
		if slug == "" {
			continue
		}
		name, _ := m["display_name"].(string)
		if name == "" {
			name = slug
		}
		out = append(out, Model{Slug: slug, DisplayName: name, Efforts: reasoningLevels(m["supported_reasoning_levels"])})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// reasoningLevels reads supported_reasoning_levels, which is either a list of
// effort names or a list of {"effort": "...", "description": "..."} objects.
func reasoningLevels(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		switch x := item.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			if e, ok := x["effort"].(string); ok && e != "" {
				out = append(out, e)
			}
		}
	}
	return out
}
