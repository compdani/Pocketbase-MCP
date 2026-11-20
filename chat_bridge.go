package pocketbasemcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/protocol"
	"github.com/compdani/pocketbase-mcp/schema"
)

// DefaultChatPrefix is the chat API root used when ChatOptions.Prefix is empty.
// It deliberately lives outside the MCP mount so the two surfaces cannot
// shadow each other.
const DefaultChatPrefix = "/api/mcp/chat/v1"

// chatSessionExpiredMessage is the only text a revalidation failure
// contributes to a chat turn: it tells the model to ask for a new sign-in
// without naming any internal detail.
const chatSessionExpiredMessage = "Your session has expired. Sign in again."

// errChatSessionExpired is the executor-level failure of a Tools call after the
// caller's credentials stopped verifying.
var errChatSessionExpired = errors.New("pocketbase-mcp: chat session is no longer valid")

// ChatOptions configures Server.ChatHandler. Chat is off unless a host builds a
// handler; it shares the MCP tool registry, policy engine and audit stream, and
// can narrow what MCP offers but never widen it.
type ChatOptions struct {
	// Namespace identifies the host application in every chat ownership key.
	// Required: an empty namespace would put every app in one partition.
	Namespace string
	// Prefix is the chat API root, default DefaultChatPrefix. It must not equal
	// or live under any MCP path (canonical or alias), and no MCP path may live
	// under it, so one mount can never shadow the other.
	Prefix string
	// Runner executes turns. Required.
	Runner *chat.Runner
	// Store persists conversations, turns and events. Required, and it must be
	// the store the Runner uses.
	Store chat.Store
	// Router supplies provider/model choices and resolves a turn's provider
	// when a catalog is configured. Nil keeps the runner's single-provider
	// path.
	Router chat.ModelRouter
	// ProviderAdmin, when set with AdminAuth, mounts the superuser provider
	// management routes. The adapter builds it; a host normally leaves it nil.
	ProviderAdmin chat.ProviderAdmin
	// AdminAuth authenticates the provider admin routes. It must be a
	// superuser-only authenticator and is only used for those routes.
	AdminAuth chat.Authenticator
	// Models is the model allowlist reported by GET /status.
	Models []string
	// MaxBodyBytes bounds a chat request body. Zero means the chat default.
	MaxBodyBytes int64
	// MaxMessageChars bounds one user message in runes. Zero means the chat
	// default.
	MaxMessageChars int
	// HeartbeatInterval is the SSE heartbeat period. Zero means the chat default.
	HeartbeatInterval time.Duration
	// Policy is an optional built-in or host-registered policy the caller must
	// satisfy to use chat at all. Empty accepts any principal the server already
	// accepts (auth collection / superuser rules still apply).
	Policy string
	// Proposals offers write tools on chat as proposals the user must approve.
	// The Runner and Store must both support proposals. Config.AllowWrites is
	// required unless the only chat writes are editor tools.
	Proposals bool
	// AutoApprove lets a tool declared Approval "auto" run without review. It
	// requires Proposals, and it never relaxes AllowWrites, the tool's policy or
	// PocketBase rules.
	AutoApprove bool
	// AllowArgumentEdits lets a user edit a pending proposal's arguments before
	// approving it. The executor validates the edit against the same tool schema,
	// may not change what the change applies to, and the store records a new
	// revision so the decision still addresses exactly what was reviewed.
	AllowArgumentEdits bool
	// DecisionTimeout bounds how long applying one approved proposal may take.
	// Zero means the chat default (30s). Execution is detached from the request,
	// so a client disconnect cannot leave it half-recorded.
	DecisionTimeout time.Duration
	// TargetCheck validates a client-supplied target reference for a caller.
	// Nil means targets are unsupported.
	TargetCheck func(r *http.Request, c chat.Caller, target string) error
	// Uploads serves the attachment endpoints and accepts attachments on a
	// turn. The Store and Runner must both support uploads, or ChatHandler
	// refuses to start.
	Uploads bool
	// MaxUploadBytes bounds one uploaded file. Zero means the chat default
	// (10 MiB).
	MaxUploadBytes int64
	// MaxAttachmentsPerTurn bounds the uploads one user message may reference.
	// Zero means the chat default (5).
	MaxAttachmentsPerTurn int
	// MaxUploadsPerConversation bounds how many uploads one conversation may
	// keep. Zero means the chat default (50).
	MaxUploadsPerConversation int
	// TurnContext accepts a page context object on each turn (the screen,
	// selection, editor revision and the like). The runner's Setup hook
	// receives it; without a hook it is appended to the system prompt as
	// data. Off means a turn with a context is rejected.
	TurnContext bool
	// MaxContextBytes bounds one turn's context. Zero means the chat default
	// (16 KiB).
	MaxContextBytes int
	// StoreContext keeps each turn's context with its user message (a
	// "context" part the transcript returns), and hands the conversation's
	// earlier contexts to the Setup hook (TurnInput.EarlierContexts). Use it
	// for references a conversation accumulates. It needs TurnContext.
	StoreContext bool
	// Imports serves POST /conversations/actions/import for threads a browser kept
	// (plain user and assistant text). Server-side imports use
	// chat.ImportConversation with the store directly.
	Imports bool
	// MaxImportBytes bounds one import request. Zero means 8 MiB.
	MaxImportBytes int64
	// AcceptOtherImages also stores BMP, TIFF, ICO, HEIC and AVIF images
	// (chat.OtherImageTypes). They reach a model only when its provider can
	// take them; otherwise it is told the file was omitted.
	AcceptOtherImages bool
}

// ChatHandler builds the chat HTTP handler. It authenticates a bearer token the
// way /mcp does, then applies the chat policy; every tool call re-authenticates
// and re-checks the tool listing, so a permission change applies to the next
// step.
func (s *Server) ChatHandler(opts ChatOptions) (http.Handler, error) {
	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultChatPrefix
	}
	if err := s.validateChatOptions(opts, prefix); err != nil {
		return nil, err
	}
	var proposals chat.ProposalStore
	if opts.Proposals {
		// validateChatOptions proved the assertion, so the handler always gets
		// the same store the runner records proposals in.
		proposals, _ = opts.Store.(chat.ProposalStore)
	}
	var uploads chat.UploadStore
	if opts.Uploads {
		// validateChatOptions proved the assertion.
		uploads, _ = opts.Store.(chat.UploadStore)
	}
	return chat.NewHandler(chat.HandlerConfig{
		Prefix:                    prefix,
		Runner:                    opts.Runner,
		Store:                     opts.Store,
		Auth:                      &chatAuthenticator{s: s, namespace: opts.Namespace, policy: opts.Policy, writes: opts.Proposals, autoApprove: opts.AutoApprove},
		Router:                    opts.Router,
		ProviderAdmin:             opts.ProviderAdmin,
		AdminAuth:                 opts.AdminAuth,
		Models:                    opts.Models,
		MaxBodyBytes:              opts.MaxBodyBytes,
		MaxMessageChars:           opts.MaxMessageChars,
		HeartbeatInterval:         opts.HeartbeatInterval,
		TargetCheck:               opts.TargetCheck,
		Proposals:                 proposals,
		AllowArgumentEdits:        opts.AllowArgumentEdits,
		DecisionTimeout:           opts.DecisionTimeout,
		Uploads:                   uploads,
		MaxUploadBytes:            opts.MaxUploadBytes,
		MaxAttachmentsPerTurn:     opts.MaxAttachmentsPerTurn,
		MaxUploadsPerConversation: opts.MaxUploadsPerConversation,
		TurnContext:               opts.TurnContext,
		MaxContextBytes:           opts.MaxContextBytes,
		StoreContext:              opts.StoreContext,
		Imports:                   opts.Imports,
		MaxImportBytes:            opts.MaxImportBytes,
		AcceptOtherImages:         opts.AcceptOtherImages,
	})
}

// validateChatOptions checks the parts ChatHandler owns before the chat package
// validates its own configuration.
func (s *Server) validateChatOptions(opts ChatOptions, prefix string) error {
	if strings.TrimSpace(opts.Namespace) == "" {
		return errors.New("pocketbase-mcp: chat requires a namespace")
	}
	if err := s.validateChatPrefix(prefix); err != nil {
		return err
	}
	if err := ValidatePolicyName(opts.Policy, s.cfg.Policies); err != nil {
		return fmt.Errorf("pocketbase-mcp: chat policy: %w", err)
	}
	if opts.AutoApprove && !opts.Proposals {
		return errors.New("pocketbase-mcp: chat auto-approve requires proposals")
	}
	if opts.Proposals {
		// Review is the only chat path to a write, so a host that could offer a
		// server write must enable writes. A host that only offers editor tools
		// needs no AllowWrites: an editor patch writes nothing on the server.
		if !s.cfg.AllowWrites && s.chatCouldOfferServerWrite() {
			return errors.New("pocketbase-mcp: chat proposals require writes (Config.AllowWrites)")
		}
		if opts.Runner == nil || !opts.Runner.ProposalsEnabled() {
			return errors.New("pocketbase-mcp: chat proposals require a runner configured with a proposal store")
		}
		// The handler answers the decision endpoints from the store it is given,
		// so it has to be a proposal store too; otherwise a decision could never
		// reach the proposal the turn recorded.
		if _, ok := opts.Store.(chat.ProposalStore); !ok {
			return errors.New("pocketbase-mcp: chat proposals require a store that implements chat.ProposalStore")
		}
	}
	if opts.Uploads {
		// An upload the handler could store but the runner could never send
		// would silently disappear from the model's view, so both halves must
		// be configured together.
		if _, ok := opts.Store.(chat.UploadStore); !ok {
			return errors.New("pocketbase-mcp: chat uploads require a store that implements chat.UploadStore")
		}
		if opts.Runner == nil || !opts.Runner.UploadsEnabled() {
			return errors.New("pocketbase-mcp: chat uploads require a runner configured with an upload store")
		}
	}
	if opts.Runner != nil && opts.Store != nil && !opts.Runner.UsesStore(opts.Store) {
		return errors.New("pocketbase-mcp: chat store must be the store the runner uses (and its proposal and upload store when those are on)")
	}
	return nil
}

// chatCouldOfferServerWrite reports whether any caller could get a server-side
// chat write (editor tools excluded). It is deliberately conservative: it only
// decides whether proposals need Config.AllowWrites.
func (s *Server) chatCouldOfferServerWrite() bool {
	for _, ct := range s.cfg.CustomTools {
		if ct.onChatChannel() && ct.writeEffect() && !ct.editorTool() {
			return true
		}
	}
	for _, c := range s.cfg.EffectiveManifest().Collections {
		for _, op := range c.Operations {
			if isWriteOp(op) {
				return true
			}
		}
	}
	return false
}

// validateChatPrefix refuses a chat mount that could shadow, or be shadowed by,
// any MCP mount. Both directions matter: a prefix under /mcp would be served by
// the MCP handler, and an MCP path under the prefix would be swallowed by the
// chat mux.
func (s *Server) validateChatPrefix(prefix string) error {
	for _, p := range s.Paths() {
		if prefix == p || strings.HasPrefix(prefix, p) || strings.HasPrefix(p, prefix) {
			return fmt.Errorf("pocketbase-mcp: chat prefix %q collides with the MCP path %q", prefix, p)
		}
	}
	return nil
}

// ChatAuthenticate resolves a chat caller from a request's bearer token. It is
// the full gate (authenticate, accept, chat policy) and is what an adapter
// should use when it can hand over the request.
//
// It returns chat.ErrUnauthorized for missing or invalid credentials, so a
// caller cannot distinguish them, and chat.ErrForbidden when the policy denies
// an otherwise valid principal.
func (s *Server) ChatAuthenticate(r *http.Request, opts ChatOptions) (chat.Caller, error) {
	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultChatPrefix
	}
	if err := s.validateChatOptions(opts, prefix); err != nil {
		return chat.Caller{}, err
	}
	p, err := s.authenticateChat(r, opts.Policy)
	if err != nil {
		return chat.Caller{}, err
	}
	return s.chatCallerWithWrites(p, r, opts.Namespace, opts.Policy, opts.Proposals, opts.AutoApprove), nil
}

// ChatCaller builds a read-only chat.Caller for a principal the adapter already
// resolved. It does NO authentication and NO chat-policy check; prefer
// ChatAuthenticate when the request still carries the token. Without p.Token,
// tools cannot re-authenticate per call. The mount is always this server's
// path.
func (s *Server) ChatCaller(p Principal, r *http.Request, namespace string) chat.Caller {
	return s.chatCaller(p, r, namespace)
}

// authenticateChat is the shared identity gate of both chat entry points.
func (s *Server) authenticateChat(r *http.Request, policy string) (Principal, error) {
	token := extractToken(r)
	if token == "" {
		return Principal{}, chat.ErrUnauthorized
	}
	p, err := s.host.Authenticate(token)
	if err != nil {
		return Principal{}, chat.ErrUnauthorized
	}
	p.Token = token
	if !s.acceptPrincipal(p) {
		return Principal{}, chat.ErrUnauthorized
	}
	if !EvaluatePolicy(policy, p, s.cfg.Policies) {
		return Principal{}, chat.ErrForbidden
	}
	return p, nil
}

// chatAuthenticator is the chat package's Authenticator over this server.
type chatAuthenticator struct {
	s         *Server
	namespace string
	policy    string
	// writes and autoApprove come from ChatOptions and are fixed for the
	// handler's lifetime, so a request can never widen them.
	writes      bool
	autoApprove bool
}

func (a *chatAuthenticator) Authenticate(r *http.Request) (chat.Caller, error) {
	p, err := a.s.authenticateChat(r, a.policy)
	if err != nil {
		return chat.Caller{}, err
	}
	return a.s.chatCallerWithWrites(p, r, a.namespace, a.policy, a.writes, a.autoApprove), nil
}

// ChatAdminAuthenticator returns the superuser-only authenticator an adapter
// uses for the provider admin routes. It is exported so an adapter can pass it
// through ChatOptions without knowing the internal type.
func (s *Server) ChatAdminAuthenticator(namespace string) chat.Authenticator {
	return &chatAdminAuthenticator{s: s, namespace: namespace}
}

// chatAdminAuthenticator is the superuser-only gate for provider management.
// It deliberately bypasses the chat policy and the AllowSuperusers opt-in: a
// deployment can keep chat closed to superusers and still let one administer
// providers. It grants no chat access and no tools.
type chatAdminAuthenticator struct {
	s         *Server
	namespace string
}

func (a *chatAdminAuthenticator) Authenticate(r *http.Request) (chat.Caller, error) {
	token := extractToken(r)
	if token == "" {
		return chat.Caller{}, chat.ErrUnauthorized
	}
	p, err := a.s.host.Authenticate(token)
	if err != nil || !p.IsSuperuser {
		return chat.Caller{}, chat.ErrForbidden
	}
	return chat.Caller{
		Owner: chat.OwnerKey{
			Namespace:        a.namespace,
			Mount:            a.s.cfg.Path,
			AuthCollectionID: p.CollectionID,
			UserID:           p.ID,
		},
		Expires: tokenExpiry(token),
	}, nil
}

// chatCaller binds one principal to one owner key, credential expiry and
// read-only tool executor. The owner's auth collection uses the stable
// collection ID, never the mutable name.
func (s *Server) chatCaller(p Principal, r *http.Request, namespace string) chat.Caller {
	return s.chatCallerWithWrites(p, r, namespace, "", false, false)
}

// chatCallerWithWrites binds the executor's chat write policy as well. writes
// offers write tools as proposals; autoApprove lets Approval "auto" tools run
// without review. Both are host configuration, not request input.
func (s *Server) chatCallerWithWrites(p Principal, r *http.Request, namespace, policy string, writes, autoApprove bool) chat.Caller {
	token := p.Token
	return chat.Caller{
		Owner: chat.OwnerKey{
			Namespace:        namespace,
			Mount:            s.cfg.Path,
			AuthCollectionID: p.CollectionID,
			UserID:           p.ID,
		},
		Tools:    &chatToolExecutor{s: s, base: p, token: token, baseURL: s.baseURL(r), policy: policy, writes: writes, autoApprove: autoApprove},
		Expires:  tokenExpiry(token),
		Identity: p,
	}
}

// TurnPrincipal returns the caller of a chat turn, for a RunnerConfig.Setup
// hook: the principal the chat authenticator put in TurnInput.Identity. ok is
// false for a turn from another authenticator. The principal carries the
// caller's token; never store or log it. The adapters' ResolveTurnCaller goes
// one step further and returns the signed-in user, checked again.
func TurnPrincipal(in chat.TurnInput) (Principal, bool) {
	p, ok := in.Identity.(Principal)
	return p, ok && p.ID != ""
}

// tokenExpiry reads the exp claim of an already verified JWT. It only decodes
// the payload segment for scheduling purposes; verification happened in
// Host.Authenticate. An undecodable token yields the zero time ("unknown").
func tokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some issuers pad the segment; the padded alphabet is the same bytes.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0).UTC()
}

// chatToolExecutor is the ToolExecutor for one chat caller. It holds the raw
// token in memory only (never logged, audited or serialized) so it can
// revalidate the PocketBase identity before every tool listing and call.
type chatToolExecutor struct {
	s       *Server
	base    Principal
	token   string
	baseURL string
	// writes reports that the host offers this caller's write tools as
	// proposals. It is fixed when the caller is built, never request input.
	writes bool
	// autoApprove lets an Approval "auto" tool run without review. It only
	// matters when writes is set.
	autoApprove bool
	// policy is the chat access policy, re-evaluated before every tool so a
	// role removed mid-turn stops the next step.
	policy string
	seq    atomic.Uint64
}

var _ chat.ToolExecutor = (*chatToolExecutor)(nil)
var _ chat.ProposalPlanner = (*chatToolExecutor)(nil)
var _ chat.ProposalExecutor = (*chatToolExecutor)(nil)

// staleProposalMessage is the one refusal a proposal gets when the tool, the
// manifest or the caller's permissions changed after it was reviewed. It tells
// the user what to do without naming anything internal.
const staleProposalMessage = "This change is out of date. Ask for a new proposal."

// current revalidates the caller and returns the principal to act as. With a
// token, the host authenticates it again, so a rotated token key (revocation)
// or a removed auth collection stops the very next tool. Without a token (an
// adapter using ChatCaller) the already-resolved principal is used.
func (e *chatToolExecutor) current() (Principal, bool) {
	if e.token == "" {
		if e.base.ID == "" {
			return Principal{}, false
		}
		return e.base, true
	}
	p, err := e.s.host.Authenticate(e.token)
	if err != nil || !e.s.acceptPrincipal(p) || !EvaluatePolicy(e.policy, p, e.s.cfg.Policies) {
		return Principal{}, false
	}
	p.Token = e.token
	return p, true
}

// Tools lists the current chat tool surface. It is recomputed on every step
// (never cached) so a permission change takes effect on the next model step.
func (e *chatToolExecutor) Tools(ctx context.Context) ([]provider.ToolDef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, ok := e.current()
	if !ok {
		return nil, errChatSessionExpired
	}
	defs := e.s.listChatTools(p, e.writes)
	out := make([]provider.ToolDef, 0, len(defs))
	for _, d := range defs {
		out = append(out, provider.ToolDef{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema})
	}
	return out, nil
}

// writeCall is one validated chat write call. Plan, Check, Execute and Revise
// all build it through resolveWrite, so every stage applies the same gates.
type writeCall struct {
	name       string
	desc       chatWriteDescriptor
	collection string
	// editorID is the document an editor tool edits. It is empty for a server
	// tool, and it is the proposal's target for an editor tool.
	editorID  string
	args      map[string]any
	principal Principal
}

// resolveWrite authenticates the caller against the host, checks that name is
// still a chat write tool listed for this caller, and validates the arguments
// against the tool's own schema and per-collection operation gate. A tool
// outside the caller's surface is refused exactly like a hidden tool.
func (e *chatToolExecutor) resolveWrite(ctx context.Context, name string, args json.RawMessage) (writeCall, error) {
	desc, isWrite := e.s.chatWriteTool(name)
	if !isWrite {
		return writeCall{}, chat.ToolError{Message: "Tool is unavailable."}
	}
	if err := ctx.Err(); err != nil {
		return writeCall{}, chat.ToolError{Message: "Tool failed."}
	}
	p, ok := e.current()
	if !ok {
		return writeCall{}, chat.ToolError{Message: chatSessionExpiredMessage}
	}
	if !e.s.toolListedWithWrites(ctx, p, "chat", name, e.writes) {
		return writeCall{}, chat.ToolError{Message: "Tool is unavailable."}
	}
	in, err := chatToolArgs(args)
	if err != nil {
		return writeCall{}, err
	}

	collection := desc.collection
	if desc.custom != nil {
		if msg := validateCustomArgs(desc.custom, in); msg != "" {
			return writeCall{}, chat.ToolError{Message: msg}
		}
		if msg := runToolValidate(ctx, desc.custom, p, in); msg != "" {
			return writeCall{}, chat.ToolError{Message: "Invalid params: " + msg}
		}
		if desc.editor() {
			// The editor a patch targets is part of the proposal identity, so
			// it must name something even though the schema only requires a
			// string.
			editorID := strings.TrimSpace(getStringArg(in, "editorId"))
			if editorID == "" {
				return writeCall{}, chat.ToolError{Message: "Invalid params: /editorId must be a non-empty string"}
			}
			return writeCall{name: name, desc: desc, editorID: editorID, args: in, principal: p}, nil
		}
	} else if msg := validateGeneratedArgs(desc, in); msg != "" {
		return writeCall{}, chat.ToolError{Message: msg}
	} else if desc.action != "" && collection == "" {
		// Generic mode: the arguments name the collection, and callTool's own
		// resolution and per-collection gate decide whether it is writable.
		cfg, found := e.s.host.LookupCollection(strings.TrimSpace(getStringArg(in, "collection")))
		if !found {
			return writeCall{}, chat.ToolError{Message: "Unknown or unavailable collection. Call describe_access to list collections."}
		}
		if yes, msg := e.s.canInvoke(p, cfg, desc.action); !yes {
			return writeCall{}, chat.ToolError{Message: msg}
		}
		collection = cfg.Name
	}
	return writeCall{name: name, desc: desc, collection: collection, args: in, principal: p}, nil
}

// Plan classifies one model call. Reads and unknown names return nil (Call
// decides them); an unavailable write is refused like a hidden tool; a usable
// write returns a validated draft, unless the host auto-approves that tool.
func (e *chatToolExecutor) Plan(ctx context.Context, name string, args json.RawMessage) (*chat.ProposalDraft, error) {
	ctx = withToolListingCache(ctx)
	desc, isWrite := e.s.chatWriteTool(name)
	if !isWrite {
		return nil, nil
	}
	autoApproved := desc.custom != nil && desc.custom.approvalAuto() && e.autoApprove
	if desc.custom != nil && desc.custom.Prepare != nil && !autoApproved {
		// The proposal stores what Prepare returns, so that is what gets
		// validated, reviewed and approved.
		p, ok := e.current()
		if !ok {
			return nil, chat.ToolError{Message: chatSessionExpiredMessage}
		}
		if !e.s.toolListedWithWrites(ctx, p, "chat", name, e.writes) {
			return nil, chat.ToolError{Message: "Tool is unavailable."}
		}
		in, err := chatToolArgs(args)
		if err != nil {
			return nil, err
		}
		prepared, msg := runToolPrepare(ctx, desc.custom, p, in)
		if msg != "" {
			return nil, chat.ToolError{Message: "Invalid params: " + msg}
		}
		if args, err = json.Marshal(prepared); err != nil {
			return nil, chat.ToolError{Message: "Invalid tool arguments."}
		}
	}
	call, err := e.resolveWrite(ctx, name, args)
	if err != nil {
		return nil, err
	}
	if autoApproved {
		// The host chose review-free execution for this tool, and every other
		// gate still applies when Call runs it.
		return nil, nil
	}
	// The "proposed" audit line is written once the runner has stored the
	// proposal, with its id, so a failed store leaves no line behind.
	audit := func(collection, action string) func(chat.Proposal) {
		return func(stored chat.Proposal) {
			e.s.emitAudit("chat", call.principal, name, "write", collection, action, "proposed", time.Now(), "proposal-"+stored.ID, e.s.auditPayload(call.args))
		}
	}
	if call.desc.editor() {
		// An editor tool never runs here: its call becomes a proposal whose
		// approval only authorizes the browser to apply the draft patch.
		return &chat.ProposalDraft{
			Tool:             name,
			ToolFingerprint:  call.desc.fingerprint(),
			ManifestRevision: e.s.manifest().Revision(),
			Location:         chat.ProposalLocationEditor,
			Target:           call.editorID,
			Arguments:        call.canonicalArgs(),
			Summary:          call.desc.chatEditSummary(call.editorID, call.args),
			Warnings:         runToolWarn(ctx, call.desc.custom, call.principal, call.args),
			OnStored:         audit("", ""),
		}, nil
	}
	return &chat.ProposalDraft{
		Tool:             name,
		ToolFingerprint:  call.desc.fingerprint(),
		ManifestRevision: e.s.manifest().Revision(),
		Location:         chat.ProposalLocationServer,
		Target:           call.desc.chatWriteTarget(call.args, call.collection),
		Arguments:        call.canonicalArgs(),
		Summary:          call.desc.chatWriteSummary(call.args, call.collection),
		Warnings:         runToolWarn(ctx, call.desc.custom, call.principal, call.args),
		OnStored:         audit(call.collection, call.desc.action),
	}, nil
}

// checkProposal reports whether p could still run for this caller now: the tool
// is still offered, its definition and the manifest are unchanged since review,
// and the arguments still validate. For an editor proposal, applicability to the
// editor content is the browser's call.
func (e *chatToolExecutor) checkProposal(ctx context.Context, p chat.Proposal) (writeCall, error) {
	call, err := e.resolveWrite(ctx, p.Tool, p.Arguments)
	if err != nil {
		return writeCall{}, err
	}
	if fingerprint := call.desc.fingerprint(); fingerprint == "" || fingerprint != p.ToolFingerprint {
		return writeCall{}, chat.ToolError{Message: staleProposalMessage}
	}
	if e.s.manifest().Revision() != p.ManifestRevision {
		return writeCall{}, chat.ToolError{Message: staleProposalMessage}
	}
	if call.desc.editor() && call.editorID != p.Target {
		// A patch that now names another document is not the change the user
		// approved.
		return writeCall{}, chat.ToolError{Message: staleProposalMessage}
	}
	// A generated tool names its effect in its arguments. An update that moved
	// to another record, or a create that moved to another collection, is not
	// the change the user reviewed.
	if call.desc.custom == nil && p.Target != "" && call.desc.chatWriteTarget(call.args, call.collection) != p.Target {
		return writeCall{}, chat.ToolError{Message: staleProposalMessage}
	}
	return call, nil
}

// Check implements chat.ProposalExecutor.
func (e *chatToolExecutor) Check(ctx context.Context, p chat.Proposal) error {
	ctx = withToolListingCache(ctx)
	_, err := e.checkProposal(ctx, p)
	return err
}

// Execute rechecks an approved server proposal for the freshly authenticated
// caller, then runs it once. Editor proposals never run here. A ToolError means
// nothing applied; a timeout or cancellation after dispatch is ErrOutcomeUnknown.
func (e *chatToolExecutor) Execute(ctx context.Context, p chat.Proposal) (json.RawMessage, error) {
	ctx = withToolListingCache(ctx)
	if p.Location == chat.ProposalLocationEditor {
		return nil, chat.ToolError{Message: "An editor change is applied in the browser, not on the server."}
	}
	call, err := e.checkProposal(ctx, p)
	if err != nil {
		return nil, err
	}
	return e.run(ctx, call, true, fmt.Sprintf("proposal-%s", p.ID))
}

// Revise implements chat.ProposalExecutor: edited arguments are validated
// exactly as Plan validates a fresh call, and neither a generated tool nor an
// editor tool may be retargeted to another collection, record or document.
func (e *chatToolExecutor) Revise(ctx context.Context, p chat.Proposal, args json.RawMessage) (json.RawMessage, string, error) {
	ctx = withToolListingCache(ctx)
	call, err := e.resolveWrite(ctx, p.Tool, args)
	if err != nil {
		return nil, "", err
	}
	if call.desc.editor() && call.editorID != p.Target {
		return nil, "", chat.ToolError{Message: "The edited arguments cannot change what the change applies to."}
	}
	if call.desc.custom == nil && p.Target != "" && call.desc.chatWriteTarget(call.args, call.collection) != p.Target {
		return nil, "", chat.ToolError{Message: "The edited arguments cannot change what the change applies to."}
	}
	canonical, hash, err := chat.HashArguments(args)
	if err != nil {
		return nil, "", chat.ToolError{Message: "Invalid tool arguments."}
	}
	return canonical, hash, nil
}

// Call runs one chat tool through the same pipeline as MCP. A write that needs
// review is refused here as a safety net; the runner plans every write first.
func (e *chatToolExecutor) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	ctx = withToolListingCache(ctx)
	p, ok := e.current()
	if !ok {
		return nil, chat.ToolError{Message: chatSessionExpiredMessage}
	}
	in, err := chatToolArgs(args)
	if err != nil {
		return nil, err
	}
	if e.writeNeedsReview(ctx, p, name) {
		return nil, chat.ToolError{Message: "This change needs review."}
	}
	desc, _ := e.s.chatWriteTool(name)
	return e.run(ctx, writeCall{name: name, desc: desc, args: in, principal: p}, false, fmt.Sprintf("chat-%d", e.seq.Add(1)))
}

// run executes a resolved call and returns the model's JSON or a user-safe
// error. approved bypasses review; only Execute passes true, never Call.
func (e *chatToolExecutor) run(ctx context.Context, call writeCall, approved bool, requestID string) (json.RawMessage, error) {
	if call.desc.editor() {
		// An editor effect has no server handler at all. Approval authorizes
		// the browser, never this service, so no entry point may run it. A tool
		// this caller cannot see stays indistinguishable from a hidden one.
		if !e.s.toolListedWithWrites(ctx, call.principal, "chat", call.name, e.writes) {
			return nil, chat.ToolError{Message: "Tool is unavailable."}
		}
		return nil, chat.ToolError{Message: "This change needs review."}
	}
	if !approved && e.writeNeedsReview(ctx, call.principal, call.name) {
		return nil, chat.ToolError{Message: "This change needs review."}
	}
	result, userErr, rpcErr := e.s.callToolWithWrites(ctx, call.principal, e.baseURL, "chat", e.writes, call.name, call.args, requestID)
	if rpcErr != nil {
		return nil, chat.ToolError{Message: chatRPCErrorMessage(rpcErr)}
	}
	if userErr != "" {
		// A dispatch that ended without a definite answer may still have
		// applied, so it can never be recorded as a plain failure.
		if ctx.Err() != nil || userErr == customToolTimeoutMessage {
			return nil, fmt.Errorf("%s: %w", userErr, chat.ErrOutcomeUnknown)
		}
		if _, refused := result.(toolRefusal); refused && approved {
			return nil, chat.RefusedError{Message: userErr}
		}
		return nil, chat.ToolError{Message: userErr}
	}
	return chatToolResult(result)
}

// writeNeedsReview reports whether name is a write tool this caller would have
// to review. A tool the caller cannot currently use is not reported: the channel
// gate refuses it as unavailable, which is the established message.
func (e *chatToolExecutor) writeNeedsReview(ctx context.Context, p Principal, name string) bool {
	desc, ok := e.s.chatWriteTool(name)
	if !ok {
		return false
	}
	if desc.custom != nil && desc.custom.approvalAuto() && e.autoApprove {
		return false
	}
	return e.s.toolListedWithWrites(ctx, p, "chat", name, e.writes)
}

// chatToolArgs decodes one tool call's arguments the one way both Plan and Call
// accept them: an absent or null value is an empty object, and anything that is
// not an object is the same user-safe refusal.
func chatToolArgs(args json.RawMessage) (map[string]any, error) {
	in := map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, chat.ToolError{Message: "Invalid tool arguments."}
		}
	}
	if in == nil {
		in = map[string]any{}
	}
	return in, nil
}

// validateCustomArgs validates a custom tool's arguments against the schema the
// call itself uses. The message matches what chatRPCErrorMessage renders for the
// same -32602 error, so the model sees no difference between a refused call and
// a refused proposal.
func validateCustomArgs(t *ToolSpec, args map[string]any) string {
	inputSchema := t.InputSchema
	if inputSchema == nil {
		inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	sch, err := schema.Compile(inputSchema)
	if err == nil {
		err = sch.Validate(args)
	}
	if err != nil {
		return "Invalid params: " + err.Error()
	}
	return ""
}

// validateGeneratedArgs checks the shape of a generated create/update/delete
// call before it becomes a proposal, so a call the tool would refuse (no id, no
// data object, unknown top-level keys) fails now instead of after approval.
// Field values inside data are left to PocketBase, which validates them when
// the approved change runs.
func validateGeneratedArgs(desc chatWriteDescriptor, args map[string]any) string {
	if desc.custom != nil || desc.action == "" {
		return ""
	}
	allowed := map[string]bool{"collection": desc.collection == ""}
	if desc.action != "create" {
		allowed["id"] = true
		if strings.TrimSpace(getStringArg(args, "id")) == "" {
			return "Invalid params: /id must be a non-empty string"
		}
	}
	if desc.action != "delete" {
		allowed["data"] = true
		if _, ok := args["data"].(map[string]any); !ok {
			return "Invalid params: /data must be an object"
		}
	}
	for key := range args {
		if !allowed[key] {
			return "Invalid params: /" + key + " is not allowed"
		}
	}
	return ""
}

// canonicalArgs is the validated argument object as JSON. A proposal binds
// this, never the provider's raw bytes, so `null` arguments (accepted as {})
// cannot fail the turn when the proposal is stored.
func (c writeCall) canonicalArgs() json.RawMessage {
	raw, err := json.Marshal(c.args)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// chatRPCErrorMessage renders a JSON-RPC tool failure as user-safe text. The
// data of an invalid-params error is our own schema pointer path, not host
// detail, so it is safe to include.
func chatRPCErrorMessage(e *protocol.Error) string {
	if e == nil {
		return "Tool is unavailable."
	}
	if detail, ok := e.Data.(string); ok && detail != "" {
		return e.Message + ": " + detail
	}
	return e.Message
}

// chatToolResult turns a callTool result into the JSON the model receives.
//
// For a protocol.ToolResult the structured content is the payload; a text-only
// result contributes its text (raw when it is JSON itself). Binary and linked
// resources are not sent to the model in v1.1 and become a short note instead.
func chatToolResult(result any) (json.RawMessage, error) {
	tr, ok := result.(protocol.ToolResult)
	if !ok {
		return json.Marshal(result)
	}
	text, omitted := toolResultText(tr)
	if tr.IsError {
		if text == "" {
			text = "Tool failed."
		}
		return nil, chat.ToolError{Message: text}
	}
	if tr.StructuredContent != nil {
		return json.Marshal(tr.StructuredContent)
	}
	if text == "" {
		if omitted {
			return json.RawMessage(`{"omitted":"binary resource"}`), nil
		}
		return json.RawMessage("null"), nil
	}
	if !omitted && json.Valid([]byte(text)) {
		return json.RawMessage(text), nil
	}
	return json.Marshal(text)
}

// toolResultText joins the text parts of a result and reports whether any
// non-text part (an embedded resource or resource link) was dropped.
func toolResultText(tr protocol.ToolResult) (text string, omitted bool) {
	var parts []string
	for _, c := range tr.Content {
		switch part := c.(type) {
		case protocol.ToolContent:
			if part.Type == "text" && part.Text != "" {
				parts = append(parts, part.Text)
				continue
			}
			if part.Text != "" && part.Type == "" {
				parts = append(parts, part.Text)
				continue
			}
			omitted = true
		case map[string]any:
			if t, ok := part["text"].(string); ok && t != "" {
				if typ, ok := part["type"].(string); !ok || typ == "text" {
					parts = append(parts, t)
					continue
				}
			}
			omitted = true
		default:
			omitted = true
		}
	}
	return strings.Join(parts, "\n"), omitted
}
