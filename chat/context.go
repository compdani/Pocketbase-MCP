package chat

import (
	"context"
	"errors"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// Context window bookkeeping. The runner keeps, per conversation and in
// memory only:
//
//   - a calibration ratio, actual input tokens / estimate, from the usage
//     providers report, so estimates track the real tokenizer;
//   - the last request's size and window (contextRecord), which is what
//     ContextStatus reports to clients.
//
// Both are process-local like the rest of the runner's scheduling state. After
// a restart the ratio starts at 1 and the status is estimated from the store.

const (
	minCalibration = 0.5
	maxCalibration = 2.0
	// overflowShrink is the budget factor a retry after a context overflow
	// applies, compounding if a turn overflows again on a later step.
	overflowShrink = 0.7
)

// contextRecord is the last request of one conversation.
type contextRecord struct {
	tokens   int
	window   int
	provider string
	model    string
	// estimated is true when tokens is the runner's estimate rather than a
	// provider's count.
	estimated bool
	at        time.Time
}

// ContextStatus is how full a conversation's context is, for a client's
// context indicator. Tokens and Window are zero when unknown.
type ContextStatus struct {
	// Tokens is the input of the latest request, or an estimate of the next
	// one for a conversation this process hasn't run a turn in.
	Tokens int `json:"tokens"`
	// Window is the model's input window; zero means unknown, and a client
	// should then show no indicator.
	Window int `json:"window"`
	// Estimated is true when Tokens is an estimate.
	Estimated bool `json:"estimated"`
	// SummarizedThrough is the id of the last message the conversation's
	// summary covers, or "" when there is none. Messages up to it are given
	// to the model as the summary.
	SummarizedThrough string     `json:"summarizedThrough,omitempty"`
	SummarizedAt      *time.Time `json:"summarizedAt,omitempty"`
	// Compacting is true while a summary is being written.
	Compacting bool `json:"compacting"`
	// CanCompact reports that summaries are on for this conversation's store.
	CanCompact bool `json:"canCompact"`
}

func conversationKey(owner OwnerKey, conversationID string) string {
	return owner.String() + "\x00" + conversationID
}

// windowFor resolves a model's input window: the choice's own (from the
// catalog), then RunnerConfig.ContextWindow, then the built-in defaults.
func (r *Runner) windowFor(resolved ResolvedModel) int {
	if resolved.Choice.ContextWindow > 0 {
		return resolved.Choice.ContextWindow
	}
	if r.cfg.ContextWindow > 0 {
		return r.cfg.ContextWindow
	}
	return DefaultContextWindow(resolved.Model)
}

// outputReserve is what the budget keeps free for the reply: the turn's
// output bound, or a default that never takes more than a quarter of a small
// window.
func outputReserve(maxOutputTokens, window int) int {
	if maxOutputTokens > 0 {
		return maxOutputTokens
	}
	if window > 0 && window/4 < defaultOutputReserve {
		return window / 4
	}
	return defaultOutputReserve
}

// heldTokens is what a conversation holds, for the context indicator and the
// summary trigger: the request's size, or, when the history had to be cut to
// fit, the calibrated estimate of the whole history.
func heldTokens(j *turnJob, request int, ratio float64) int {
	if j.shrunk {
		if full := int(float64(j.lastFull) * ratio); full > request {
			return full
		}
	}
	return request
}

// contextMargin is the safety margin of a window.
func (r *Runner) contextMargin(window int) int {
	if r.cfg.ContextMargin > 0 {
		return r.cfg.ContextMargin
	}
	margin := window / 20
	if margin < minContextMargin {
		margin = minContextMargin
	}
	return margin
}

// budgetFor is the history budget of the job's next request, in estimate
// units. overhead is the estimate of the system prompt and tool definitions.
func (r *Runner) budgetFor(j *turnJob, overhead int) historyBudget {
	b := historyBudget{
		keepFullToolTurns:   r.cfg.KeepFullToolTurns,
		maxToolResultTokens: r.cfg.MaxToolResultTokens,
		imageTokens:         r.cfg.ImageTokenEstimate,
	}
	window := j.settings.contextWindow
	if window <= 0 {
		if j.overflowBase > 0 {
			b.tokens = int(float64(j.overflowBase)*j.shrink) - overhead
			if b.tokens < 1 {
				b.tokens = 1
			}
		}
		return b
	}
	available := window - outputReserve(j.settings.maxOutputTokens, window) - r.contextMargin(window)
	available = int(float64(available)*j.shrink) - int(float64(overhead)*r.calibration(j.owner, j.conversationID))
	tokens := int(float64(available) / r.calibration(j.owner, j.conversationID))
	if tokens < 1 {
		// A known window with no room left still means "shrink everything",
		// never "unknown".
		tokens = 1
	}
	b.tokens = tokens
	return b
}

// calibration returns the conversation's actual/estimate ratio.
func (r *Runner) calibration(owner OwnerKey, conversationID string) float64 {
	r.ctxMu.Lock()
	defer r.ctxMu.Unlock()
	if ratio, ok := r.ctxRatios[conversationKey(owner, conversationID)]; ok {
		return ratio
	}
	return 1
}

func clampRatio(v float64) float64 {
	if v < minCalibration {
		return minCalibration
	}
	if v > maxCalibration {
		return maxCalibration
	}
	return v
}

// observeUsage records a provider's input-token count for the job's last
// request: it corrects the conversation's calibration and its status.
func (r *Runner) observeUsage(j *turnJob, inputTokens int64) {
	if inputTokens <= 0 {
		return
	}
	key := conversationKey(j.owner, j.conversationID)
	j.usageSeen = true
	r.ctxMu.Lock()
	defer r.ctxMu.Unlock()
	if j.lastEstimate > 0 {
		sample := clampRatio(float64(inputTokens) / float64(j.lastEstimate))
		if old, ok := r.ctxRatios[key]; ok {
			sample = clampRatio((old + sample) / 2)
		}
		r.ctxRatios[key] = sample
	}
	ratio := 1.0
	if v, ok := r.ctxRatios[key]; ok {
		ratio = v
	}
	r.ctxRecords[key] = contextRecord{
		tokens:   heldTokens(j, int(inputTokens), ratio),
		window:   j.settings.contextWindow,
		provider: j.providerID,
		model:    j.model,
		at:       r.now(),
	}
}

// recordStep records a finished step's request size when the provider
// reported no usage for it, from the calibrated estimate.
func (r *Runner) recordStep(j *turnJob) {
	if j.usageSeen {
		// The provider's own count for this step is already recorded.
		return
	}
	key := conversationKey(j.owner, j.conversationID)
	r.ctxMu.Lock()
	defer r.ctxMu.Unlock()
	ratio := 1.0
	if v, ok := r.ctxRatios[key]; ok {
		ratio = v
	}
	r.ctxRecords[key] = contextRecord{
		tokens:    heldTokens(j, int(float64(j.lastEstimate)*ratio), ratio),
		window:    j.settings.contextWindow,
		provider:  j.providerID,
		model:     j.model,
		estimated: true,
		at:        r.now(),
	}
}

// retryOverflow decides whether a failed step is retried with a smaller
// history: only for a context overflow, and only once per step.
func (r *Runner) retryOverflow(j *turnJob, step int, err error) bool {
	if !provider.IsContextOverflow(err) || j.retriedStep == step {
		return false
	}
	j.retriedStep = step
	j.shrink *= overflowShrink
	if j.settings.contextWindow <= 0 && j.overflowBase == 0 {
		// The window is unknown, so nothing was budgeted: the request that
		// failed is the best measure of it, and the retry shrinks from there.
		j.overflowBase = j.lastEstimate
	}
	key := conversationKey(j.owner, j.conversationID)
	r.ctxMu.Lock()
	old, ok := r.ctxRatios[key]
	if !ok {
		old = 1
	}
	r.ctxRatios[key] = clampRatio(old * 1.25)
	r.ctxMu.Unlock()
	r.cfg.Logger.Info("chat: the model's context window overflowed; retrying with a shorter history",
		"conversation", j.conversationID, "turn", j.turnID, "provider", j.providerID, "model", j.model,
		"estimate", j.lastEstimate, "error", err.Error())
	return true
}

// providerFailure is the failure code of a provider error.
func providerFailure(err error) string {
	switch {
	case provider.IsContextOverflow(err):
		return FailureContextTooLong
	case errors.Is(err, errOutputLimit):
		return FailureOutputLimit
	case errors.Is(err, errContentFiltered):
		return FailureContentFiltered
	}
	if code := provider.FailureKind(err); code != "" {
		return code
	}
	return FailureProvider
}

// ContextStatus reports how full a conversation's context is. The caller has
// already checked that the conversation is owner's.
func (r *Runner) ContextStatus(ctx context.Context, owner OwnerKey, conversationID string) (ContextStatus, error) {
	key := conversationKey(owner, conversationID)
	var status ContextStatus
	summary, err := r.latestSummary(ctx, owner, conversationID)
	if err != nil {
		return ContextStatus{}, err
	}
	if summary.ThroughMessageID != "" {
		status.SummarizedThrough = summary.ThroughMessageID
		at := summary.CreatedAt
		status.SummarizedAt = &at
	}
	status.CanCompact = r.CompactionEnabled()

	r.ctxMu.Lock()
	rec, haveRecord := r.ctxRecords[key]
	ratio, haveRatio := r.ctxRatios[key]
	_, status.Compacting = r.compacting[key]
	r.ctxMu.Unlock()
	if !haveRatio {
		ratio = 1
	}
	if haveRecord && rec.window > 0 && (summary.ThroughMessageID == "" || !summary.CreatedAt.After(rec.at)) {
		status.Tokens, status.Window, status.Estimated = rec.tokens, rec.window, rec.estimated
		return status, nil
	}

	// No request since the last summary (or since this process started):
	// estimate the next one with the model the conversation last used, or
	// the default.
	resolved, err := r.resolveForConversation(ctx, rec)
	if err != nil {
		return status, nil
	}
	window := r.windowFor(resolved)
	if window <= 0 {
		return status, nil
	}
	limit := r.cfg.MaxHistoryMessages
	if !r.historyExplicit {
		limit = windowedMaxHistoryMessages
	}
	msgs, err := r.recentMessages(ctx, owner, conversationID, limit)
	if err != nil {
		return ContextStatus{}, err
	}
	if summary.ThroughMessageID != "" {
		msgs = messagesAfter(msgs, summary.ThroughMessageID)
	}
	res := buildHistory(historyPlan{msgs: msgs, summary: summary.Text})
	status.Tokens = int(float64(res.tokens+estimateText(r.cfg.System)) * ratio)
	status.Window = window
	status.Estimated = true
	return status, nil
}

// resolveForConversation resolves the model a conversation last used, or the
// default when that is unknown or no longer available.
func (r *Runner) resolveForConversation(ctx context.Context, rec contextRecord) (ResolvedModel, error) {
	if rec.model != "" {
		if resolved, err := r.resolveTarget(ctx, SubmitOptions{Provider: rec.provider, Model: rec.model}); err == nil {
			return resolved, nil
		}
	}
	return r.resolveTarget(ctx, SubmitOptions{})
}

// recentMessages reads the last limit messages of a conversation.
func (r *Runner) recentMessages(ctx context.Context, owner OwnerKey, conversationID string, limit int) ([]Message, error) {
	if fast, ok := r.cfg.Store.(RecentMessageStore); ok {
		return fast.RecentMessages(ctx, owner, conversationID, limit)
	}
	var all []Message
	page := Page{Limit: maxPageLimit}
	for {
		items, next, err := r.cfg.Store.ListMessages(ctx, owner, conversationID, page)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

// ContextManagement reports whether the default model has a known context
// window, which is when a client's context indicator has something to show.
func (r *Runner) ContextManagement(ctx context.Context) bool {
	resolved, err := r.resolveTarget(ctx, SubmitOptions{})
	if err != nil {
		return false
	}
	return r.windowFor(resolved) > 0
}
