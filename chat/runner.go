package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// Failure codes recorded on a turn that does not complete. They are part of the
// wire contract: a client may branch on Code, never on Message.
const (
	// FailureMaxSteps reports a turn that needed more model steps than allowed.
	FailureMaxSteps = "max_steps"
	// FailureProvider reports a provider error, a truncated stream or a request
	// the provider could not serve.
	FailureProvider = "provider_error"
	// FailureTimeout reports a turn that exceeded its own time budget.
	FailureTimeout = "timeout"
	// FailureAuthExpired reports a turn that reached the caller's credential
	// deadline. The worker must not outlive the token it is acting with.
	FailureAuthExpired = "auth_expired"
	// FailureInterrupted reports a turn ended by server shutdown or restart
	// recovery. The terminal event is turn.failed, as protocol v1 requires.
	FailureInterrupted = "interrupted"
	// FailureInternal reports an unexpected server-side failure.
	FailureInternal = "internal_error"
)

const (
	defaultMaxSteps             = 8
	defaultTurnTimeout          = 2 * time.Minute
	defaultMaxConcurrentPerUser = 2
	defaultMaxConcurrentGlobal  = 16
	defaultMaxHistoryMessages   = 50
	// defaultProposalTTL bounds how long a stored proposal may be decided when
	// RunnerConfig.ProposalTTL leaves it unset.
	defaultProposalTTL = 24 * time.Hour

	// cancelReasonUser is the recorded reason of an explicit user stop.
	cancelReasonUser = "user"
)

// failureMessage is the only text a failure code contributes to the record.
// Messages are deliberately generic: raw provider or driver text never reaches
// the model, the transcript or the browser.
func failureMessage(code string) string {
	switch code {
	case FailureMaxSteps:
		return "The turn exceeded the maximum number of steps."
	case FailureProvider:
		return "The model request failed."
	case FailureTimeout:
		return "The turn timed out."
	case FailureAuthExpired:
		return "Authorization expired before the turn finished."
	case FailureInterrupted:
		return "The turn was interrupted."
	default:
		return "The turn failed."
	}
}

// ErrShutdown reports that a runner no longer accepts turns.
var ErrShutdown = errors.New("chat: runner is shut down")

var (
	errStreamTruncated = errors.New("chat: provider stream ended without a done chunk")
	errStreamFailed    = errors.New("chat: provider reported an error")
)

// RunnerConfig configures a Runner. Zero values take the documented defaults.
type RunnerConfig struct {
	Store    Store
	Provider provider.Provider
	// Model is recorded on each turn and sent with every provider request. It
	// is the legacy single-provider default; when Router is set it is only a
	// fallback for a router that has no configured default.
	Model string
	// Router, when set, resolves a per-turn provider and model from the
	// provider catalog. Provider and Model remain the legacy single-provider
	// path.
	Router ModelRouter
	// System is the system prompt sent with every provider request.
	System string
	// MaxSteps bounds model steps per turn, counting tool round trips. Zero
	// means 8.
	MaxSteps int
	// TurnTimeout bounds one turn's lifetime. Zero means 2 minutes.
	TurnTimeout time.Duration
	// MaxConcurrentPerUser bounds concurrently running turns for one owner.
	// Zero means 2.
	MaxConcurrentPerUser int
	// MaxConcurrentGlobal bounds concurrently running turns in this process.
	// Zero means 16.
	MaxConcurrentGlobal int
	// MaxHistoryMessages bounds the transcript sent to the model, counted in
	// stored messages. Zero means 50.
	MaxHistoryMessages int
	// Proposals makes the runner store a reviewed proposal instead of running
	// a call an executor plans for review. It is optional and additive: a nil
	// store leaves turns exactly as they were, and an executor that returns a
	// draft without a store configured fails the call rather than running it.
	Proposals ProposalStore
	// ProposalTTL bounds how long a stored proposal may be decided. Zero
	// means 24 hours.
	ProposalTTL time.Duration
	// Uploads resolves attachment parts in the request history. Nil means the
	// runner serves text-only turns: an attachment part is reported to the
	// model as omitted rather than fetched. It is the same store the handler
	// accepts uploads into.
	Uploads UploadStore
	// MaxAttachmentBytesPerRequest bounds the decoded attachment bytes one
	// provider request may carry, counted newest message first. Zero means
	// 20 MiB. Anything over budget, missing or unsupported is replaced with a
	// short omission note instead of failing the turn.
	MaxAttachmentBytesPerRequest int64
	// Now is the clock used for deadline arithmetic. Nil means time.Now.
	Now func() time.Time
	// Logger receives one record per failed turn with the failure code, the
	// provider id, the model and the cause. The cause never reaches the
	// transcript or the browser, which only see the generic failure message,
	// so this is where an operator finds out why a provider refused a turn.
	// Provider errors are already sanitized (credentials removed, length
	// bounded). Nil means slog.Default().
	Logger *slog.Logger
}

// Runner executes turns against a provider and a tool executor.
//
// A turn outlives the submission request, so the runner takes ownership of its
// lifetime: it runs on its own context, bounded by TurnTimeout and by the
// caller's credential deadline. Submission returns as soon as the turn is
// claimed and queued; progress is delivered through the store's event stream
// and the Subscribe notifier.
//
// The runner is single-instance by design: worker limits, the queue and the
// notifier are process-local. A second instance sharing the store would keep
// the store's correctness (the compare-and-swap operations and the active-turn
// claim) but not shared worker limits or cross-instance event delivery.
type Runner struct {
	cfg    RunnerConfig
	now    func() time.Time
	broker *broker
	// publishLocks order each turn's event appends with their publishes.
	publishLocks [64]sync.Mutex

	mu             sync.Mutex
	jobs           map[string]*turnJob
	queue          []*turnJob
	runningGlobal  int
	runningPerUser map[string]int
	closed         bool
	wg             sync.WaitGroup
}

type jobState int

const (
	jobPending jobState = iota
	jobRunning
	jobDone
	jobAborted
)

// Stop reasons recorded on a job. The first two are explicit; timeout and
// auth_expired are derived from the job context.
const (
	stopUser        = "user"
	stopInterrupted = "interrupted"
	stopAuth        = FailureAuthExpired
	stopTimeout     = FailureTimeout
)

// turnJob is the runner's bookkeeping for one submitted turn. Fields above the
// state comment are immutable after creation; the rest is guarded by Runner.mu.
type turnJob struct {
	key            string
	owner          OwnerKey
	turnID         string
	conversationID string
	provider       provider.Provider
	providerID     string
	model          string
	tools          ToolExecutor
	deadline       time.Time
	done           chan struct{}

	state     jobState
	stop      string
	cancel    context.CancelFunc
	authBound bool
}

// NewRunner validates cfg, fills defaults and returns a runner.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.Store == nil {
		return nil, errors.New("chat: runner requires a store")
	}
	if cfg.Provider == nil && cfg.Router == nil {
		return nil, errors.New("chat: runner requires a provider or a model router")
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = defaultMaxSteps
	}
	if cfg.TurnTimeout <= 0 {
		cfg.TurnTimeout = defaultTurnTimeout
	}
	if cfg.MaxConcurrentPerUser <= 0 {
		cfg.MaxConcurrentPerUser = defaultMaxConcurrentPerUser
	}
	if cfg.MaxConcurrentGlobal <= 0 {
		cfg.MaxConcurrentGlobal = defaultMaxConcurrentGlobal
	}
	if cfg.MaxHistoryMessages <= 0 {
		cfg.MaxHistoryMessages = defaultMaxHistoryMessages
	}
	if cfg.ProposalTTL <= 0 {
		cfg.ProposalTTL = defaultProposalTTL
	}
	if cfg.MaxAttachmentBytesPerRequest <= 0 {
		cfg.MaxAttachmentBytesPerRequest = defaultMaxAttachmentBytesPerRequest
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Runner{
		cfg:            cfg,
		now:            nowFn,
		broker:         newBroker(),
		jobs:           make(map[string]*turnJob),
		runningPerUser: make(map[string]int),
	}, nil
}

// turnKey scopes a turn to its owner as well as its ID, because record IDs are
// never sufficient identity.
func turnKey(owner OwnerKey, turnID string) string {
	return owner.String() + "\x00" + turnID
}

// ProposalsEnabled reports whether the runner can store proposals. A host uses
// it to refuse a chat configuration that offers write tools it could never
// record.
func (r *Runner) ProposalsEnabled() bool { return r.cfg.Proposals != nil }

// UsesStore reports whether s is the store this runner writes turns to, and,
// when proposals or uploads are configured, also the store it uses for them.
// The HTTP handler reads and writes through s, so any mismatch would split one
// conversation across two stores.
func (r *Runner) UsesStore(s Store) bool {
	if any(r.cfg.Store) != any(s) {
		return false
	}
	if r.cfg.Proposals != nil && any(r.cfg.Proposals) != any(s) {
		return false
	}
	if r.cfg.Uploads != nil && any(r.cfg.Uploads) != any(s) {
		return false
	}
	return true
}

// UploadsEnabled reports whether the runner can resolve attachments. A host
// uses it to refuse an upload surface whose turns could never send a file.
func (r *Runner) UploadsEnabled() bool { return r.cfg.Uploads != nil }

// SupportsAttachment reports whether the configured provider can send one
// attachment of the given MIME type. A provider that declares no attachment
// support supports none, so the upload surface degrades to text only instead of
// silently dropping files.
func (r *Runner) SupportsAttachment(mimeType string) bool {
	if r.cfg.Router != nil {
		return r.cfg.Router.SupportsAttachment(mimeType)
	}
	support, ok := r.cfg.Provider.(provider.AttachmentSupport)
	return ok && support.SupportsAttachment(mimeType)
}

// Choices lists the selectable provider/model pairs. A runner without a router
// has the legacy single choice, or none when no model is configured.
func (r *Runner) Choices(ctx context.Context) ([]ModelChoice, error) {
	if r.cfg.Router != nil {
		return r.cfg.Router.Choices(ctx)
	}
	if strings.TrimSpace(r.cfg.Model) == "" {
		return nil, nil
	}
	return []ModelChoice{{
		ID:      ChoiceID("", r.cfg.Model),
		Model:   r.cfg.Model,
		Label:   r.cfg.Model,
		Default: true,
	}}, nil
}

// DefaultChoice returns the choice a turn uses when it names neither provider
// nor model.
func (r *Runner) DefaultChoice(ctx context.Context) (ModelChoice, error) {
	if r.cfg.Router != nil {
		return r.cfg.Router.Default(ctx)
	}
	if strings.TrimSpace(r.cfg.Model) == "" || r.cfg.Provider == nil {
		return ModelChoice{}, ErrNoModelConfigured
	}
	return ModelChoice{
		ID:      ChoiceID("", r.cfg.Model),
		Model:   r.cfg.Model,
		Label:   r.cfg.Model,
		Default: true,
	}, nil
}

// Router returns the configured model router, or nil for the legacy path.
func (r *Runner) Router() ModelRouter { return r.cfg.Router }

// SubmitOptions is one turn submission. Text is the only required field; every
// other field has a neutral default: the runner's model, no deduplication, no
// tools, no attachments and an unknown credential expiry.
type SubmitOptions struct {
	// Text is the user message appended to the transcript before the turn
	// starts.
	Text string
	// Attachments are the uploads the user message references, in order. The
	// stored message gets one attachment part per entry; the bytes stay in the
	// upload store and are resolved again for every provider request.
	Attachments []Upload
	// IdempotencyKey deduplicates a submission whose response was lost. An
	// empty key means the submission is not deduplicated.
	IdempotencyKey string
	// Provider selects a catalog provider. Empty means the default provider,
	// or the legacy single provider when no router is configured.
	Provider string
	// Model overrides the runner's default model for this turn. Empty means the
	// provider's default model, or the runner's configured model on the legacy
	// path.
	Model string
	// Target is an already-resolved choice. The HTTP handler sets it after
	// resolving and validating attachments; leaving it nil lets the runner
	// resolve through its router.
	Target *ResolvedModel
	// Tools is the caller-bound executor the turn may call. Nil means the turn
	// runs without tools.
	Tools ToolExecutor
	// Deadline is the caller's credential expiry; zero means unknown. A
	// deadline earlier than the turn timeout ends the turn as auth_expired, so
	// a worker never acts with an expired token.
	Deadline time.Time
}

// SubmitWithOptions is the option-taking form of Submit. It exists because a
// submission carries per-request bindings (model, tool executor, credential
// deadline) that must not be stored in the runner's shared configuration.
//
// SubmitWithOptions claims a turn, stores the user message and the queued
// event, and starts or queues the work. It returns as soon as the claim is
// stored: created is false when the idempotency key already identified a turn,
// in which case nothing new runs.
func (r *Runner) SubmitWithOptions(ctx context.Context, owner OwnerKey, conversationID string, opts SubmitOptions) (Turn, bool, error) {
	if err := owner.Validate(); err != nil {
		return Turn{}, false, err
	}
	// A retry with the same key must return the stored turn even if the
	// provider or model it named has since disappeared. Do this before
	// resolving the choice so a stale retry cannot fail.
	if opts.IdempotencyKey != "" {
		if finder, ok := r.cfg.Store.(IdempotentTurnStore); ok {
			turn, found, err := finder.FindTurnByIdempotencyKey(ctx, owner, conversationID, opts.IdempotencyKey)
			if err != nil {
				return Turn{}, false, err
			}
			if found {
				return turn, false, nil
			}
		}
	}
	resolved, err := r.resolveTarget(ctx, opts)
	if err != nil {
		return Turn{}, false, err
	}
	turn, created, err := r.cfg.Store.CreateTurn(ctx, owner, Turn{
		ConversationID: conversationID,
		IdempotencyKey: opts.IdempotencyKey,
		Provider:       resolved.Choice.Provider,
		Model:          resolved.Model,
	})
	if err != nil {
		return Turn{}, false, err
	}
	if !created {
		return turn, false, nil
	}

	storeCtx := context.Background()
	parts := make([]Part, 0, 1+len(opts.Attachments))
	parts = append(parts, Part{Type: PartText, Text: opts.Text})
	for _, u := range opts.Attachments {
		parts = append(parts, Part{
			Type:     PartAttachment,
			UploadID: u.ID,
			Name:     u.Name,
			MIME:     u.MIME,
		})
	}
	if _, err := r.cfg.Store.AppendMessage(storeCtx, owner, Message{
		ConversationID: conversationID,
		TurnID:         turn.ID,
		Role:           RoleUser,
		Parts:          parts,
	}); err != nil {
		r.releaseFailedSubmit(owner, conversationID, turn.ID)
		return turn, true, err
	}
	if _, err := r.appendTurnEvent(storeCtx, owner, turn.ID, EventTurnQueued, nil); err != nil {
		r.releaseFailedSubmit(owner, conversationID, turn.ID)
		return turn, true, err
	}

	j := &turnJob{
		key:            turnKey(owner, turn.ID),
		owner:          owner,
		turnID:         turn.ID,
		conversationID: conversationID,
		provider:       resolved.Provider,
		providerID:     resolved.Choice.Provider,
		model:          resolved.Model,
		tools:          opts.Tools,
		deadline:       opts.Deadline,
		done:           make(chan struct{}),
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		r.finishJob(j, TurnStateQueued, TurnStateInterrupted, FailureInterrupted)
		return Turn{}, false, ErrShutdown
	}
	r.jobs[j.key] = j
	r.queue = append(r.queue, j)
	r.startReadyLocked()
	r.mu.Unlock()

	return turn, true, nil
}

// resolveTarget turns a submission's provider/model (or an already-resolved
// target) into the concrete provider and wire model for the turn. It runs
// before CreateTurn so an unknown choice never leaves a failed turn behind.
func (r *Runner) resolveTarget(ctx context.Context, opts SubmitOptions) (ResolvedModel, error) {
	if opts.Target != nil {
		if opts.Target.Provider == nil {
			return ResolvedModel{}, ErrModelUnknown
		}
		target := *opts.Target
		if target.Model == "" {
			target.Model = target.Choice.Model
		}
		return target, nil
	}
	if r.cfg.Router != nil {
		return r.cfg.Router.Resolve(ctx, strings.TrimSpace(opts.Provider), strings.TrimSpace(opts.Model))
	}
	if strings.TrimSpace(opts.Provider) != "" {
		return ResolvedModel{}, ErrModelUnknown
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = strings.TrimSpace(r.cfg.Model)
	}
	if model == "" || r.cfg.Provider == nil {
		return ResolvedModel{}, ErrNoModelConfigured
	}
	return ResolvedModel{
		Choice: ModelChoice{
			ID:      ChoiceID("", model),
			Model:   model,
			Label:   model,
			Default: true,
		},
		Provider: r.cfg.Provider,
		Model:    model,
	}, nil
}

// Submit is SubmitWithOptions without a per-turn model override.
//
// deadline is the caller's credential expiry. A non-zero deadline earlier than
// the turn timeout ends the turn as auth_expired, so a worker never acts with
// an expired token.
func (r *Runner) Submit(ctx context.Context, owner OwnerKey, conversationID, text, idempotencyKey string, tools ToolExecutor, deadline time.Time) (Turn, bool, error) {
	return r.SubmitWithOptions(ctx, owner, conversationID, SubmitOptions{
		Text:           text,
		IdempotencyKey: idempotencyKey,
		Tools:          tools,
		Deadline:       deadline,
	})
}

// Cancel ends one turn.
//
// A queued turn becomes cancelled immediately. A running turn has its context
// cancelled, which reaches the provider stream and any tool call, and the
// worker stores the one turn.cancelled event; Cancel waits for that, bounded by
// ctx. A tool that already completed keeps its stored result. Cancelling a
// terminal turn is a no-op that returns the stored turn.
func (r *Runner) Cancel(ctx context.Context, owner OwnerKey, turnID string) (Turn, error) {
	if err := owner.Validate(); err != nil {
		return Turn{}, err
	}
	key := turnKey(owner, turnID)

	r.mu.Lock()
	j := r.jobs[key]
	if j != nil {
		switch j.state {
		case jobPending:
			j.state = jobAborted
			r.removeQueuedLocked(j)
			r.mu.Unlock()
			r.finishJob(j, TurnStateQueued, TurnStateCancelled, "")
			return r.cfg.Store.GetTurn(context.Background(), owner, turnID)
		case jobRunning:
			j.stop = stopUser
			cancel := j.cancel
			done := j.done
			r.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			select {
			case <-done:
			case <-ctx.Done():
			}
			return r.cfg.Store.GetTurn(context.Background(), owner, turnID)
		}
	}
	r.mu.Unlock()

	// No live job for this turn. The store is authoritative: it may be terminal
	// already, queued by another runner, or running somewhere this runner
	// cannot reach.
	turn, err := r.cfg.Store.GetTurn(ctx, owner, turnID)
	if err != nil {
		return Turn{}, err
	}
	if turn.State.Terminal() {
		return turn, nil
	}
	if turn.State == TurnStateQueued {
		r.finishJob(&turnJob{
			key:            key,
			owner:          owner,
			turnID:         turn.ID,
			conversationID: turn.ConversationID,
		}, TurnStateQueued, TurnStateCancelled, "")
		return r.cfg.Store.GetTurn(context.Background(), owner, turnID)
	}
	return turn, nil
}

// Shutdown stops accepting turns, ends queued turns as interrupted, cancels the
// running ones and waits for their workers. Each turn still ends with exactly
// one terminal event. The wait is bounded by ctx; a worker that ignores
// cancellation keeps running and the returned error reports the expiry.
func (r *Runner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	var pending, running []*turnJob
	for _, j := range r.jobs {
		switch j.state {
		case jobPending:
			j.state = jobAborted
			pending = append(pending, j)
		case jobRunning:
			j.stop = stopInterrupted
			running = append(running, j)
		}
	}
	r.queue = nil
	r.mu.Unlock()

	for _, j := range pending {
		r.finishJob(j, TurnStateQueued, TurnStateInterrupted, FailureInterrupted)
	}
	for _, j := range running {
		r.mu.Lock()
		cancel := j.cancel
		r.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- scheduling ---

func (r *Runner) startReadyLocked() {
	if r.closed {
		return
	}
	for {
		idx := r.nextStartableLocked()
		if idx < 0 {
			return
		}
		j := r.queue[idx]
		r.queue = append(r.queue[:idx], r.queue[idx+1:]...)
		r.startLocked(j)
	}
}

// nextStartableLocked returns the oldest queued job whose owner still has an
// available slot. Skipping a blocked owner lets other owners run, while each
// owner's own turns stay in submission order.
func (r *Runner) nextStartableLocked() int {
	if r.runningGlobal >= r.cfg.MaxConcurrentGlobal {
		return -1
	}
	for i, j := range r.queue {
		if j.state != jobPending {
			continue
		}
		if r.runningPerUser[j.owner.String()] >= r.cfg.MaxConcurrentPerUser {
			continue
		}
		return i
	}
	return -1
}

func (r *Runner) startLocked(j *turnJob) {
	j.state = jobRunning
	r.runningGlobal++
	r.runningPerUser[j.owner.String()]++

	// The worker's lifetime is its own: never the submission request's context.
	// It ends at the turn timeout or at the caller's credential deadline,
	// whichever comes first, and remembers which one applied.
	timeout := r.cfg.TurnTimeout
	j.authBound = false
	if !j.deadline.IsZero() {
		remaining := j.deadline.Sub(r.now())
		if remaining <= timeout {
			timeout = remaining
			j.authBound = true
		}
	}
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	j.cancel = cancel

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.runTurn(ctx, cancel, j)
	}()
}

func (r *Runner) removeQueuedLocked(target *turnJob) {
	for i, j := range r.queue {
		if j == target {
			r.queue = append(r.queue[:i], r.queue[i+1:]...)
			return
		}
	}
}

// jobDone releases the worker slot and starts whatever is next in line.
func (r *Runner) jobDone(j *turnJob) {
	r.mu.Lock()
	if j.state == jobRunning {
		key := j.owner.String()
		r.runningGlobal--
		r.runningPerUser[key]--
		if r.runningPerUser[key] <= 0 {
			delete(r.runningPerUser, key)
		}
	}
	j.state = jobDone
	delete(r.jobs, j.key)
	r.startReadyLocked()
	r.mu.Unlock()
}

// --- turn execution ---

func (r *Runner) runTurn(ctx context.Context, cancel context.CancelFunc, j *turnJob) {
	// Deferred in reverse order: recover, then release the slot, then cancel the
	// context, then wake any Cancel waiting for this job.
	defer close(j.done)
	defer cancel()
	defer r.jobDone(j)
	defer func() {
		if rec := recover(); rec != nil {
			r.failJob(j, FailureInternal, fmt.Errorf("panic: %v", rec))
		}
	}()

	bg := context.Background()
	if _, err := r.cfg.Store.TransitionTurn(bg, j.owner, j.turnID, TurnStateQueued, TurnStateRunning, nil); err != nil {
		// The turn was already ended (cancelled while queued, interrupted, or
		// finished by recovery), so it already has its one terminal event.
		return
	}
	if _, err := r.appendTurnEvent(bg, j.owner, j.turnID, EventTurnStarted, nil); err != nil {
		r.failJob(j, FailureInternal, err)
		return
	}
	if stop := r.stopKind(ctx, j); stop != "" {
		r.finishStopped(j, stop)
		return
	}

	for step := 0; ; step++ {
		if step >= r.cfg.MaxSteps {
			r.failJob(j, FailureMaxSteps, nil)
			return
		}
		if stop := r.stopKind(ctx, j); stop != "" {
			r.finishStopped(j, stop)
			return
		}

		req, err := r.buildRequest(ctx, j)
		if err != nil {
			r.failJob(j, FailureInternal, err)
			return
		}
		if j.provider == nil {
			r.failJob(j, FailureInternal, errors.New("chat: no provider resolved for the turn"))
			return
		}
		ch, err := j.provider.Stream(ctx, req)
		if err != nil {
			if stop := r.stopKind(ctx, j); stop != "" {
				r.finishStopped(j, stop)
				return
			}
			r.failJob(j, FailureProvider, err)
			return
		}

		text, calls, err := r.consume(ctx, j, ch)
		if stop := r.stopKind(ctx, j); stop != "" {
			// The stream was cut short by a stop, not by a provider fault. The
			// partial step is not committed to the transcript.
			r.finishStopped(j, stop)
			return
		}
		if err != nil {
			r.failJob(j, FailureProvider, err)
			return
		}

		parts := make([]Part, 0, 1+len(calls))
		if text != "" {
			parts = append(parts, Part{Type: PartText, Text: text})
		}
		for _, call := range calls {
			parts = append(parts, Part{
				Type:      PartToolCall,
				CallID:    call.ID,
				Tool:      call.Name,
				Arguments: call.Arguments,
			})
		}
		if len(parts) > 0 {
			if _, err := r.cfg.Store.AppendMessage(bg, j.owner, Message{
				ConversationID: j.conversationID,
				TurnID:         j.turnID,
				Role:           RoleAssistant,
				Parts:          parts,
			}); err != nil {
				r.failJob(j, FailureInternal, err)
				return
			}
		}
		if len(calls) == 0 {
			r.finishJob(j, TurnStateRunning, TurnStateCompleted, "")
			return
		}
		for _, call := range calls {
			if !r.runTool(ctx, j, call) {
				return
			}
		}
	}
}

// consume reads one provider stream, emitting text.delta and usage.updated as
// chunks arrive and collecting the tool calls the step requested.
func (r *Runner) consume(ctx context.Context, j *turnJob, ch <-chan provider.Chunk) (string, []provider.ToolCall, error) {
	var (
		text  strings.Builder
		calls []provider.ToolCall
	)
	bg := context.Background()
	for {
		var (
			chunk provider.Chunk
			ok    bool
		)
		select {
		case <-ctx.Done():
			return text.String(), calls, ctx.Err()
		case chunk, ok = <-ch:
			if !ok {
				return text.String(), calls, errStreamTruncated
			}
		}

		switch chunk.Kind {
		case provider.ChunkText:
			if chunk.Text == "" {
				continue
			}
			text.WriteString(chunk.Text)
			if _, err := r.appendTurnEvent(bg, j.owner, j.turnID, EventTextDelta, TextDelta{Text: chunk.Text}); err != nil {
				return text.String(), calls, err
			}
		case provider.ChunkToolCall:
			if chunk.ToolCall == nil {
				continue
			}
			call := *chunk.ToolCall
			call.Arguments = cloneRaw(call.Arguments)
			calls = append(calls, call)
		case provider.ChunkUsage:
			if chunk.Usage == nil {
				continue
			}
			if _, err := r.appendTurnEvent(bg, j.owner, j.turnID, EventUsageUpdated, Usage{
				InputTokens:  chunk.Usage.InputTokens,
				OutputTokens: chunk.Usage.OutputTokens,
			}); err != nil {
				return text.String(), calls, err
			}
		case provider.ChunkDone:
			return text.String(), calls, nil
		case provider.ChunkError:
			if chunk.Err != nil {
				return text.String(), calls, fmt.Errorf("%w: %w", errStreamFailed, chunk.Err)
			}
			return text.String(), calls, errStreamFailed
		default:
			return text.String(), calls, fmt.Errorf("chat: unknown chunk kind %q", string(chunk.Kind))
		}
	}
}

// runTool executes one requested tool call and records its events and result
// message. It returns false when the turn was ended and the loop must stop.
func (r *Runner) runTool(ctx context.Context, j *turnJob, call provider.ToolCall) bool {
	bg := context.Background()
	args := call.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if _, err := r.appendTurnEvent(bg, j.owner, j.turnID, EventToolStarted, ToolStarted{
		CallID:    call.ID,
		Tool:      call.Name,
		Arguments: args,
	}); err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	if stop := r.stopKind(ctx, j); stop != "" {
		r.finishStopped(j, stop)
		return false
	}

	// A write the executor wants reviewed becomes a stored proposal instead of
	// a call: the model never executes it, and the tool result tells it so.
	if draft, failure, ok := r.planTool(ctx, j, call); !ok {
		return r.failTool(ctx, j, call, failure)
	} else if draft != nil {
		return r.recordProposal(ctx, j, call, *draft, args)
	}

	result, failure, ok := r.callTool(ctx, j.tools, call)
	if !ok {
		return r.failTool(ctx, j, call, failure)
	}

	if _, err := r.cfg.Store.AppendMessage(bg, j.owner, Message{
		ConversationID: j.conversationID,
		TurnID:         j.turnID,
		Role:           RoleTool,
		Parts: []Part{{
			Type:   PartToolResult,
			CallID: call.ID,
			Tool:   call.Name,
			Result: result,
		}},
	}); err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	if _, err := r.appendTurnEvent(bg, j.owner, j.turnID, EventToolCompleted, ToolCompleted{
		CallID: call.ID,
		Tool:   call.Name,
		Result: result,
	}); err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	if stop := r.stopKind(ctx, j); stop != "" {
		// The result is already stored and reported; the turn still ends as the
		// stop demands.
		r.finishStopped(j, stop)
		return false
	}
	return true
}

// planTool asks a proposal-aware executor to classify one call. A nil draft and
// no failure means the call runs now. A failure is already reduced to user-safe
// text, exactly as callTool reduces one.
func (r *Runner) planTool(ctx context.Context, j *turnJob, call provider.ToolCall) (draft *ProposalDraft, failure string, ok bool) {
	planner, isPlanner := j.tools.(ProposalPlanner)
	if !isPlanner {
		return nil, "", true
	}
	defer func() {
		// An executor's bug must not take the worker down with it.
		if rec := recover(); rec != nil {
			draft, failure, ok = nil, toolGenericFailure, false
		}
	}()

	d, err := planner.Plan(ctx, call.Name, call.Arguments)
	if err != nil {
		if msg, safe := toolFailureMessage(err); safe {
			return nil, msg, false
		}
		return nil, toolGenericFailure, false
	}
	return d, "", true
}

// proposalPendingMessage is the tool result a model receives instead of an
// execution. The note exists because models otherwise tend to report a planned
// change as done.
const proposalPendingNote = "Not applied. The user must approve this change; do not say it was done."

// proposalReviewDisabledMessage reports a planned write on a runner that has no
// proposal store. Review is impossible there, so the call fails instead of
// running unreviewed.
const proposalReviewDisabledMessage = "Changes need review, but review isn't enabled here."

// recordProposal stores one planned call as a pending proposal, appends
// proposal.created and answers the model with a pending-review tool result.
func (r *Runner) recordProposal(ctx context.Context, j *turnJob, call provider.ToolCall, draft ProposalDraft, args json.RawMessage) bool {
	bg := context.Background()
	if r.cfg.Proposals == nil {
		return r.failTool(ctx, j, call, proposalReviewDisabledMessage)
	}
	raw := draft.Arguments
	if len(raw) == 0 {
		raw = args
	}
	canonical, hash, err := HashArguments(raw)
	if err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	stored, err := r.cfg.Proposals.CreateProposal(bg, j.owner, Proposal{
		ConversationID:   j.conversationID,
		TurnID:           j.turnID,
		CallID:           call.ID,
		Tool:             draft.Tool,
		ToolFingerprint:  draft.ToolFingerprint,
		ManifestRevision: draft.ManifestRevision,
		Location:         draft.Location,
		Target:           draft.Target,
		Arguments:        canonical,
		ArgumentsHash:    hash,
		Summary:          draft.Summary,
		ExpiresAt:        r.now().Add(r.cfg.ProposalTTL),
	})
	if err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	if draft.OnStored != nil {
		draft.OnStored(stored)
	}
	if _, err := r.appendTurnEvent(bg, j.owner, j.turnID, EventProposalCreated, ProposalEventData{Proposal: stored}); err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}

	result, err := proposalPendingResult(stored)
	if err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	if _, err := r.cfg.Store.AppendMessage(bg, j.owner, Message{
		ConversationID: j.conversationID,
		TurnID:         j.turnID,
		Role:           RoleTool,
		Parts: []Part{{
			Type:   PartToolResult,
			CallID: call.ID,
			Tool:   call.Name,
			Result: result,
		}},
	}); err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	if _, err := r.appendTurnEvent(bg, j.owner, j.turnID, EventToolCompleted, ToolCompleted{
		CallID: call.ID,
		Tool:   call.Name,
		Result: result,
	}); err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	if stop := r.stopKind(ctx, j); stop != "" {
		// The proposal and its result are stored; the turn still ends as the
		// stop demands.
		r.finishStopped(j, stop)
		return false
	}
	return true
}

// proposalPendingResult is the tool result of a call that became a proposal. It
// carries the new proposal's ID and summary so the model can refer to the
// change, and never implies that anything was applied.
func proposalPendingResult(p Proposal) (json.RawMessage, error) {
	return json.Marshal(struct {
		Status     string `json:"status"`
		ProposalID string `json:"proposalId"`
		Summary    string `json:"summary"`
		Note       string `json:"note"`
	}{
		Status:     "pending_review",
		ProposalID: p.ID,
		Summary:    p.Summary,
		Note:       proposalPendingNote,
	})
}

// failTool records one failed tool call and reports whether the turn continues.
// Cancellation is not a tool failure and ends the turn without one.
func (r *Runner) failTool(ctx context.Context, j *turnJob, call provider.ToolCall, failure string) bool {
	bg := context.Background()
	if stop := r.stopKind(ctx, j); stop != "" {
		r.finishStopped(j, stop)
		return false
	}
	payload, err := json.Marshal(map[string]string{"error": failure})
	if err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	if _, err := r.cfg.Store.AppendMessage(bg, j.owner, Message{
		ConversationID: j.conversationID,
		TurnID:         j.turnID,
		Role:           RoleTool,
		Parts: []Part{{
			Type:    PartToolResult,
			CallID:  call.ID,
			Tool:    call.Name,
			Result:  payload,
			IsError: true,
		}},
	}); err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	if _, err := r.appendTurnEvent(bg, j.owner, j.turnID, EventToolFailed, ToolFailed{
		CallID:  call.ID,
		Tool:    call.Name,
		Message: failure,
	}); err != nil {
		r.failJob(j, FailureInternal, err)
		return false
	}
	return true
}

// callTool invokes the executor with panic isolation. Every failure is reduced
// to a user-safe message: ToolError's own message, or the generic text.
func (r *Runner) callTool(ctx context.Context, exec ToolExecutor, call provider.ToolCall) (result json.RawMessage, failure string, ok bool) {
	if exec == nil {
		return nil, toolGenericFailure, false
	}
	defer func() {
		if rec := recover(); rec != nil {
			result, failure, ok = nil, toolGenericFailure, false
		}
	}()

	res, err := exec.Call(ctx, call.Name, call.Arguments)
	if err != nil {
		if msg, safe := toolFailureMessage(err); safe {
			return nil, msg, false
		}
		return nil, toolGenericFailure, false
	}
	if len(res) == 0 {
		res = json.RawMessage("null")
	}
	if !json.Valid(res) {
		// The wire contract requires a JSON result; keep the envelope valid
		// without inventing content.
		res, _ = json.Marshal(string(res))
	}
	return cloneRaw(res), "", true
}

// finishStopped ends a running turn for the stop that interrupted it.
func (r *Runner) finishStopped(j *turnJob, stop string) {
	switch stop {
	case stopUser:
		r.finishJob(j, TurnStateRunning, TurnStateCancelled, "")
	case stopInterrupted:
		r.finishJob(j, TurnStateRunning, TurnStateInterrupted, FailureInterrupted)
	case stopAuth:
		r.finishJob(j, TurnStateRunning, TurnStateFailed, FailureAuthExpired)
	default:
		r.finishJob(j, TurnStateRunning, TurnStateFailed, FailureTimeout)
	}
}

// stopKind reports why the turn must stop, or "" while it may continue. An
// explicit stop wins over an expired context, and an expiry caused by the
// caller's credential deadline is reported as auth_expired, not timeout.
func (r *Runner) stopKind(ctx context.Context, j *turnJob) string {
	r.mu.Lock()
	stop, auth := j.stop, j.authBound
	r.mu.Unlock()

	if stop != "" {
		return stop
	}
	if ctx.Err() == nil {
		return ""
	}
	if auth {
		return stopAuth
	}
	return stopTimeout
}

// finishJob stores a terminal state and its one terminal event through the
// store's atomic FinishTurn, then publishes the event. code is the failure code
// for failed and interrupted turns and is ignored otherwise.
// failJob logs why a running turn failed, then ends it with code. The stored
// turn and its terminal event keep only the generic failure message.
func (r *Runner) failJob(j *turnJob, code string, cause error) {
	attrs := []any{
		"code", code,
		"turn", j.turnID,
		"conversation", j.conversationID,
		"provider", j.providerID,
		"model", j.model,
	}
	if cause != nil {
		attrs = append(attrs, "error", cause.Error())
	}
	if code == FailureMaxSteps {
		attrs = append(attrs, "maxSteps", r.cfg.MaxSteps)
	}
	r.cfg.Logger.Warn("chat: turn failed", attrs...)
	r.finishJob(j, TurnStateRunning, TurnStateFailed, code)
}

func (r *Runner) finishJob(j *turnJob, from, to TurnState, code string) {
	typ, data := terminalPayload(to, code)
	ev, err := NewEvent(j.conversationID, j.turnID, typ, data)
	if err != nil {
		return
	}
	mu := r.publishLock(j.key)
	mu.Lock()
	defer mu.Unlock()
	_, stored, err := r.cfg.Store.FinishTurn(context.Background(), j.owner, j.turnID, from, to, ev)
	if err != nil {
		// A conflict means someone else already stored the turn's one outcome;
		// any other error leaves nothing more this worker can do.
		return
	}
	r.broker.publish(j.key, stored)
}

// releaseFailedSubmit ends a turn whose submission failed after it claimed the
// conversation's active slot. Without it the queued turn has no worker, and
// every later turn in that conversation is refused until a restart.
func (r *Runner) releaseFailedSubmit(owner OwnerKey, conversationID, turnID string) {
	r.finishJob(&turnJob{key: turnKey(owner, turnID), owner: owner, turnID: turnID, conversationID: conversationID},
		TurnStateQueued, TurnStateInterrupted, FailureInternal)
}

func terminalPayload(to TurnState, code string) (EventType, any) {
	switch to {
	case TurnStateCompleted:
		return EventTurnCompleted, nil
	case TurnStateCancelled:
		return EventTurnCancelled, TurnCancelled{Reason: cancelReasonUser}
	case TurnStateInterrupted, TurnStateFailed:
		return EventTurnFailed, TurnFailed{Code: code, Message: failureMessage(code)}
	}
	return EventTurnFailed, TurnFailed{Code: FailureInternal, Message: failureMessage(FailureInternal)}
}

func (r *Runner) appendTurnEvent(ctx context.Context, owner OwnerKey, turnID string, typ EventType, data any) (Event, error) {
	ev, err := NewEvent("", turnID, typ, data)
	if err != nil {
		return Event{}, err
	}
	key := turnKey(owner, turnID)
	// Append and publish under the turn's lock, so a decision request and the
	// worker appending to the same turn publish in sequence order. A live
	// stream ends on a sequence gap.
	mu := r.publishLock(key)
	mu.Lock()
	defer mu.Unlock()
	stored, err := r.cfg.Store.AppendEvent(ctx, owner, ev)
	if err != nil {
		return Event{}, err
	}
	r.broker.publish(key, stored)
	return stored, nil
}

// publishLock returns the lock shard that orders one turn's appends and
// publishes. Turns share shards, which only costs a little contention.
func (r *Runner) publishLock(key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &r.publishLocks[h.Sum32()%uint32(len(r.publishLocks))]
}

// --- request history ---

func (r *Runner) buildRequest(ctx context.Context, j *turnJob) (provider.Request, error) {
	messages, err := r.history(ctx, j)
	if err != nil {
		return provider.Request{}, err
	}
	var defs []provider.ToolDef
	if j.tools != nil {
		defs, err = j.tools.Tools(ctx)
		if err != nil {
			return provider.Request{}, err
		}
	}
	return provider.Request{
		Model:    j.model,
		System:   r.cfg.System,
		Messages: messages,
		Tools:    defs,
	}, nil
}

// history returns the bounded transcript in provider form. The bound counts
// stored messages and never splits an assistant tool call from its results.
// Attachment parts are resolved into bytes afterwards, newest message first, so
// the request budget protects the model input rather than the transcript.
func (r *Runner) history(ctx context.Context, j *turnJob) ([]provider.Message, error) {
	stored, err := r.allMessages(ctx, j)
	if err != nil {
		return nil, err
	}
	stored = trimHistory(stored, r.cfg.MaxHistoryMessages)
	return normalizeHistory(providerMessagesWith(stored, r.resolveAttachments(ctx, j, stored))), nil
}

// resolveAttachments reads the uploads the history window references. Newest
// messages win the byte budget; a missing upload, a type the provider cannot
// send, or a file that does not fit leaves no entry, and providerMessages turns
// that into an omission note. Only upload ids recorded in this conversation are
// ever read, so a part cannot address an arbitrary record or URL.
func (r *Runner) resolveAttachments(ctx context.Context, j *turnJob, msgs []Message) map[string][]byte {
	if r.cfg.Uploads == nil {
		return nil
	}
	owner := j.owner
	budget := r.cfg.MaxAttachmentBytesPerRequest
	if budget <= 0 {
		budget = defaultMaxAttachmentBytesPerRequest
	}
	resolved := make(map[string][]byte)
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, p := range msgs[i].Parts {
			if p.Type != PartAttachment || p.UploadID == "" {
				continue
			}
			if _, done := resolved[p.UploadID]; done {
				continue
			}
			if !supportsProviderAttachment(j.provider, p.MIME) {
				continue
			}
			u, data, err := r.cfg.Uploads.ReadUpload(ctx, owner, p.UploadID)
			if err != nil || u.ConversationID != msgs[i].ConversationID {
				continue
			}
			if int64(len(data)) > budget {
				continue
			}
			budget -= int64(len(data))
			resolved[p.UploadID] = data
		}
	}
	return resolved
}

// supportsProviderAttachment checks the provider selected for this turn, not
// the catalog's union. The upload surface uses the union because a file may be
// attached before the user picks a provider; the turn itself must only send a
// type its resolved provider understands.
func supportsProviderAttachment(p provider.Provider, mimeType string) bool {
	if p == nil {
		return false
	}
	support, ok := p.(provider.AttachmentSupport)
	return ok && support.SupportsAttachment(mimeType)
}

// attachmentOmittedNote is the only thing the model sees for an attachment it
// cannot be given. It names the file so the model can say what it did not read,
// and carries no bytes, error text or store detail.
func attachmentOmittedNote(name string) string {
	if name == "" {
		name = defaultUploadName
	}
	return "[attachment " + name + " omitted]"
}

func (r *Runner) allMessages(ctx context.Context, j *turnJob) ([]Message, error) {
	if fast, ok := r.cfg.Store.(RecentMessageStore); ok {
		// trimHistory keeps at most MaxHistoryMessages, so reading more is
		// wasted work on every model step.
		return fast.RecentMessages(ctx, j.owner, j.conversationID, r.cfg.MaxHistoryMessages)
	}
	var all []Message
	page := Page{Limit: maxPageLimit}
	for {
		items, next, err := r.cfg.Store.ListMessages(ctx, j.owner, j.conversationID, page)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if next == "" {
			return all, nil
		}
		page.Cursor = next
	}
}

// trimHistory cuts the oldest messages so at most max remain, then advances the
// cut past any tool result that would otherwise be orphaned. max <= 0 keeps
// everything.
func trimHistory(msgs []Message, max int) []Message {
	if max <= 0 || len(msgs) <= max {
		return msgs
	}
	cut := len(msgs) - max
	for cut < len(msgs) && msgs[cut].Role == RoleTool {
		cut++
	}
	return msgs[cut:]
}

// providerMessages converts stored messages to the provider port's shape. Each
// tool result becomes its own tool message, because that is how providers
// address results.
//
// Notices are framework notes appended from outside the turn (a decision can
// land while the turn is still running), so they are held back and emitted only
// once the run of tool messages ends. A notice therefore never separates an
// assistant message's tool calls from their results, which providers reject.
func providerMessages(msgs []Message) []provider.Message {
	return providerMessagesWith(msgs, nil)
}

// providerMessagesWith is providerMessages plus resolved attachment bytes, keyed
// by upload id. An attachment part whose upload is absent is replaced with an
// omission note in the message text, so the model knows a file was referenced
// and never receives content the server could not vouch for.
func providerMessagesWith(msgs []Message, attachments map[string][]byte) []provider.Message {
	out := make([]provider.Message, 0, len(msgs))
	var notices []provider.Message
	flushNotices := func() {
		out = append(out, notices...)
		notices = nil
	}
	for _, m := range msgs {
		if m.Role != RoleTool && len(notices) > 0 {
			flushNotices()
		}
		switch m.Role {
		case RoleUser:
			var (
				b     strings.Builder
				atts  []provider.Attachment
				notes []string
			)
			for _, p := range m.Parts {
				switch p.Type {
				case PartText:
					b.WriteString(p.Text)
				case PartAttachment:
					data, ok := attachments[p.UploadID]
					if !ok {
						notes = append(notes, attachmentOmittedNote(p.Name))
						continue
					}
					atts = append(atts, provider.Attachment{Name: p.Name, MIME: p.MIME, Data: data})
				}
			}
			for _, note := range notes {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(note)
			}
			out = append(out, provider.Message{Role: provider.RoleUser, Text: b.String(), Attachments: atts})
		case RoleAssistant:
			var (
				b     strings.Builder
				calls []provider.ToolCall
			)
			for _, p := range m.Parts {
				switch p.Type {
				case PartText:
					b.WriteString(p.Text)
				case PartToolCall:
					calls = append(calls, provider.ToolCall{
						ID:        p.CallID,
						Name:      p.Tool,
						Arguments: cloneRaw(p.Arguments),
					})
				}
			}
			out = append(out, provider.Message{
				Role:      provider.RoleAssistant,
				Text:      b.String(),
				ToolCalls: calls,
			})
		case RoleTool:
			for _, p := range m.Parts {
				if p.Type != PartToolResult {
					continue
				}
				out = append(out, provider.Message{
					Role:        provider.RoleTool,
					ToolCallID:  p.CallID,
					ToolName:    p.Tool,
					ToolResult:  cloneRaw(p.Result),
					ToolIsError: p.IsError,
				})
			}
		case RoleNotice:
			// A notice is framework-generated, not user speech: it is sent as a
			// user-role message because that is the only position every provider
			// accepts for context the model must treat as a statement of fact.
			var b strings.Builder
			for _, p := range m.Parts {
				if p.Type != PartProposalOutcome {
					continue
				}
				b.WriteString(p.Text)
			}
			if b.Len() == 0 {
				continue
			}
			notices = append(notices, provider.Message{Role: provider.RoleUser, Text: b.String()})
		}
	}
	flushNotices()
	return out
}

// normalizeHistory drops tool calls without a stored result and results whose
// call is not in the history. A turn cancelled mid tool-call leaves such a
// pair behind; providers reject it, and the model should not see a call it can
// never receive an answer to.
func normalizeHistory(msgs []provider.Message) []provider.Message {
	resolved := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		if m.Role == provider.RoleTool {
			resolved[m.ToolCallID] = true
		}
	}

	out := make([]provider.Message, 0, len(msgs))
	declared := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case provider.RoleAssistant:
			calls := make([]provider.ToolCall, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				if !resolved[c.ID] {
					continue
				}
				declared[c.ID] = true
				calls = append(calls, c)
			}
			m.ToolCalls = calls
			if m.Text == "" && len(calls) == 0 {
				continue
			}
			out = append(out, m)
		case provider.RoleTool:
			if !declared[m.ToolCallID] {
				continue
			}
			out = append(out, m)
		default:
			out = append(out, m)
		}
	}
	return out
}
