package chat

import (
	"context"
	"errors"
	"strings"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// ModelChoice is one selectable (provider, model) pair. It is what /status
// advertises and what a turn submission names. ID is opaque to clients; the
// canonical form is providerID + "::" + model, but callers must not parse it
// themselves.
type ModelChoice struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Label is the display name of the model. Empty means the model id.
	Label string `json:"label,omitempty"`
	// Group is the provider's display name, so a picker can group choices.
	Group string `json:"group,omitempty"`
	// Default marks the choice a turn uses when it names neither provider nor
	// model. At most one choice is marked.
	Default bool `json:"default,omitempty"`
	// AttachmentTypes lists the canonical MIME types this choice can send.
	// Absent means none.
	AttachmentTypes []string `json:"attachmentTypes,omitempty"`
	// Efforts lists the reasoning efforts a turn may request for this choice,
	// lowest first. Absent means the model takes none.
	Efforts []string `json:"efforts,omitempty"`
	// ContextWindow is the model's input window in tokens. Absent means
	// unknown: the runner then bounds history by message count only.
	ContextWindow int `json:"contextWindow,omitempty"`
}

// ResolvedModel is a concrete provider instance and wire model for one turn.
// It is produced by a ModelRouter before the turn is stored, so an unknown or
// ambiguous choice is a request error rather than a failed turn.
type ResolvedModel struct {
	Choice   ModelChoice
	Provider provider.Provider
	// Model is the wire model name sent to Provider. It normally equals
	// Choice.Model; a router may normalize it.
	Model string
}

// ModelRouter resolves public model choices to provider instances. It is
// implemented by the provider catalog and may be left nil, in which case the
// runner uses its single configured Provider and Model.
type ModelRouter interface {
	// Choices lists enabled, ready choices in display order.
	Choices(ctx context.Context) ([]ModelChoice, error)
	// Default returns the configured default choice, or the first ready choice
	// when no default is set.
	Default(ctx context.Context) (ModelChoice, error)
	// Resolve maps a provider id and model to a concrete provider. Empty
	// provider and model mean the default choice. Empty provider with a model
	// uses the default provider, then the unique provider offering the model.
	Resolve(ctx context.Context, providerID, model string) (ResolvedModel, error)
	// SupportsAttachment reports whether at least one ready choice can send the
	// MIME type. The upload surface uses the union; a turn is then checked
	// against its resolved choice.
	SupportsAttachment(mimeType string) bool
}

// Errors returned by a ModelRouter. The HTTP layer maps all of them to a 400
// invalid-request response; they never reach the model or the browser verbatim.
var (
	// ErrNoModelConfigured reports that no provider or model is configured.
	ErrNoModelConfigured = errors.New("chat: no model provider is configured")
	// ErrModelUnknown reports an unknown provider, model or pair.
	ErrModelUnknown = errors.New("chat: unknown model choice")
	// ErrModelAmbiguous reports a model offered by more than one provider when
	// the request did not name a provider.
	ErrModelAmbiguous = errors.New("chat: model is offered by several providers")
	// ErrEffortUnsupported reports a reasoning effort the resolved model does
	// not list in its choice's Efforts.
	ErrEffortUnsupported = errors.New("chat: reasoning effort is not offered for this model")
)

// ChoiceID builds the opaque public id for a provider and model. Provider ids
// cannot contain "::", so a split at the first separator is unambiguous.
func ChoiceID(providerID, model string) string {
	return providerID + "::" + model
}

// SplitChoiceID reverses ChoiceID. ok is false when id has no separator or a
// missing half.
func SplitChoiceID(id string) (providerID, model string, ok bool) {
	providerID, model, ok = strings.Cut(id, "::")
	if !ok || providerID == "" || model == "" {
		return "", "", false
	}
	return providerID, model, true
}
