package chat

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// Title choice values. A TitleRouter reports one of these, or a choice id
// (providerID + "::" + model), which can never collide with them.
const (
	// TitleChoiceAuto titles a conversation with the model of the turn that
	// started it.
	TitleChoiceAuto = "auto"
	// TitleChoiceOff leaves conversations untitled until a user renames them.
	TitleChoiceOff = "off"
)

// TitleConfig turns on automatic conversation titles: when a turn is submitted
// to a conversation without a title, the runner asks a model for a short title
// from the user's message, in parallel with the turn, and stores it unless the
// conversation was titled meanwhile.
type TitleConfig struct {
	// Disabled turns titling off. A nil *TitleConfig is also off; this field
	// lets a host opt out where an adapter would otherwise enable titles.
	Disabled bool
	// Model picks the model for one title, given the turn's own model.
	// ok=false skips the title. Nil means the turn's model.
	Model func(ctx context.Context, turn ResolvedModel) (ResolvedModel, bool)
	// Timeout bounds one title request. Zero means 20 seconds.
	Timeout time.Duration
	// Wait is how long a completed turn waits for its title before storing its
	// terminal event, so a client that refreshes on completion sees the title.
	// Zero means 5 seconds; a negative value never waits.
	Wait time.Duration
}

// TitleRouter is implemented by a ModelRouter that has a configured title
// model (the provider catalog does).
type TitleRouter interface {
	// TitleModel resolves the model for one title, given the turn's model.
	// ok=false means titles are off.
	TitleModel(ctx context.Context, turn ResolvedModel) (ResolvedModel, bool)
}

const (
	defaultTitleTimeout    = 20 * time.Second
	defaultTitleWait       = 5 * time.Second
	maxTitleInputRunes     = 2000
	maxGeneratedTitleRunes = 60
)

// TitleSystemPrompt is the system prompt of a title request. A scripted or
// demo provider can recognise a title request by it.
const TitleSystemPrompt = "You name chat conversations. Reply with a short title (2 to 6 words) " +
	"for a conversation that starts with the user's message below. Use the message's language. " +
	"Reply with the title only: no quotes, no trailing punctuation, no explanation."

func (c *TitleConfig) enabled() bool { return c != nil && !c.Disabled }

// startTitle begins titling the conversation when it has no title yet. It
// returns a channel closed when the attempt ends, or nil when nothing runs.
func (r *Runner) startTitle(owner OwnerKey, conversationID, text string, attachments []Upload, turn ResolvedModel) chan struct{} {
	cfg := r.cfg.Titles
	if !cfg.enabled() {
		return nil
	}
	input := titleInput(text, attachments)
	if input == "" {
		return nil
	}
	conv, err := r.cfg.Store.GetConversation(context.Background(), owner, conversationID)
	if err != nil || strings.TrimSpace(conv.Title) != "" {
		return nil
	}
	model := turn
	if cfg.Model != nil {
		picked, ok := cfg.Model(context.Background(), turn)
		if !ok {
			return nil
		}
		model = picked
	}
	if model.Provider == nil || strings.TrimSpace(model.Model) == "" {
		return nil
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTitleTimeout
	}

	done := make(chan struct{})
	r.titles.Add(1)
	go func() {
		defer r.titles.Done()
		defer close(done)
		ctx, cancel := context.WithTimeout(r.titleCtx, timeout)
		defer cancel()
		title, err := generateTitle(ctx, model, input)
		if err == nil && title == "" {
			err = errors.New("empty title")
		}
		if err != nil {
			r.cfg.Logger.Warn("chat: conversation title failed",
				"conversation", conversationID, "provider", model.Choice.Provider, "model", model.Model, "error", err.Error())
			return
		}
		// Only an untitled conversation is titled: a rename by the user (or
		// another turn's title) that landed meanwhile wins.
		current, err := r.cfg.Store.GetConversation(context.Background(), owner, conversationID)
		if err != nil || strings.TrimSpace(current.Title) != "" {
			return
		}
		if _, err := r.cfg.Store.RenameConversation(context.Background(), owner, conversationID, title); err != nil {
			r.cfg.Logger.Warn("chat: conversation title not stored", "conversation", conversationID, "error", err.Error())
		}
	}()
	return done
}

// waitTitle blocks until the job's title attempt ends or the configured wait
// elapses, whichever is first.
func (r *Runner) waitTitle(j *turnJob) {
	if j.title == nil || !r.cfg.Titles.enabled() {
		return
	}
	wait := r.cfg.Titles.Wait
	if wait < 0 {
		return
	}
	if wait == 0 {
		wait = defaultTitleWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-j.title:
	case <-timer.C:
	}
}

// titleInput is the text a title is made from: the user's message, or the
// names of the files it attached when it has no text.
func titleInput(text string, attachments []Upload) string {
	text = strings.TrimSpace(text)
	if text == "" {
		names := make([]string, 0, len(attachments))
		for _, u := range attachments {
			if name := strings.TrimSpace(u.Name); name != "" {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			return ""
		}
		text = "(Attached files: " + strings.Join(names, ", ") + ")"
	}
	if utf8.RuneCountInString(text) > maxTitleInputRunes {
		text = string([]rune(text)[:maxTitleInputRunes])
	}
	return text
}

// generateTitle asks the model for a title and cleans the reply.
func generateTitle(ctx context.Context, model ResolvedModel, input string) (string, error) {
	req := provider.Request{
		Model:    model.Model,
		System:   TitleSystemPrompt,
		Messages: []provider.Message{{Role: provider.RoleUser, Text: input}},
		// A title needs no thinking: ask for the least the model allows.
		ReasoningEffort: lowestEffort(model.Choice.Efforts),
	}
	ch, err := model.Provider.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for chunk := range ch {
		switch chunk.Kind {
		case provider.ChunkText:
			if text.Len() < 4*maxTitleInputRunes {
				text.WriteString(chunk.Text)
			}
		case provider.ChunkToolCall:
			// No tools were offered; ignore a stray call.
		case provider.ChunkError:
			if chunk.Err != nil {
				return "", chunk.Err
			}
			return "", errors.New("provider stream failed")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return CleanTitle(text.String()), nil
}

// lowestEffort returns the first (lowest) effort a choice lists, or "".
func lowestEffort(efforts []string) string {
	for _, e := range provider.ReasoningEfforts {
		if slices.Contains(efforts, e) {
			return e
		}
	}
	return ""
}

// CleanTitle turns a model's reply into a conversation title: the first
// non-empty line, without markdown, a "Title:" label, wrapping quotes or
// trailing punctuation, with whitespace collapsed and at most 60 characters
// (cut at a word when it can be).
func CleanTitle(reply string) string {
	line := ""
	for _, l := range strings.Split(reply, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	line = strings.TrimLeft(line, "#*-_> \t")
	if i := strings.Index(line, ":"); i > 0 && strings.EqualFold(strings.TrimSpace(line[:i]), "title") {
		line = line[i+1:]
	}
	line = strings.Join(strings.FieldsFunc(line, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	for {
		before := line
		line = strings.TrimSpace(line)
		line = strings.Trim(line, "\"'`*_“”‘’«»")
		line = strings.TrimRight(line, ".!,;:")
		if line == before {
			break
		}
	}
	runes := []rune(line)
	if len(runes) > maxGeneratedTitleRunes {
		cut := string(runes[:maxGeneratedTitleRunes])
		if i := strings.LastIndex(cut, " "); i > maxGeneratedTitleRunes/2 {
			cut = cut[:i]
		}
		line = strings.TrimRight(cut, " .,;:") + "…"
	}
	return line
}
