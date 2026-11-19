package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/compdani/pocketbase-mcp/chat/provider"
)

// Wire error codes. They are part of the contract: a client branches on the
// code, never on the message text.
const (
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodeNotFound         = "not_found"
	CodeActiveTurn       = "active_turn"
	CodeResyncRequired   = "resync_required"
	CodeInvalidRequest   = "invalid_request"
	CodePayloadTooLarge  = "payload_too_large"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeInternal         = "internal"
	// CodeUnsupported reports a proposal or outcome this surface cannot handle,
	// such as an editor outcome reported for a server effect.
	CodeUnsupported = "unsupported"
	// CodeStale reports a proposal that no longer matches current permissions,
	// tool definition or manifest. Nothing ran.
	CodeStale = "stale"
	// CodeConflict reports a decision that lost a race or addressed a proposal
	// that is no longer pending. The stored proposal accompanies it.
	CodeConflict = "conflict"
	// CodeInvalidArguments reports edited arguments the tool rejected.
	CodeInvalidArguments = "invalid_arguments"
	// CodeUnsupportedMedia reports an upload whose content is not one of the
	// accepted attachment types. The server, never the client, decides it.
	CodeUnsupportedMedia = "unsupported_media"
	// CodeTooManyUploads reports a conversation that reached its upload count
	// limit. Nothing was stored.
	CodeTooManyUploads = "too_many_uploads"
	// CodeInvalidAttachment reports a turn attachment reference that is not
	// the caller's upload in that conversation. It is the same answer for a
	// missing and a foreign id, so existence never leaks.
	CodeInvalidAttachment = "invalid_attachment"
	// CodeUnsupportedAttachment reports an upload the configured provider
	// cannot send, so the turn would silently lose a file.
	CodeUnsupportedAttachment = "unsupported_attachment"
	// CodeTurnRefused reports a turn the host's Setup hook refused, with the
	// host's user-facing reason. Nothing was stored.
	CodeTurnRefused = "turn_refused"
)

// Fixed, user-safe error messages. Server-internal error text is never echoed.
const (
	msgUnauthorized     = "Authentication is required."
	msgForbidden        = "The caller is not allowed to use chat."
	msgNotFound         = "Not found."
	msgActiveTurn       = "The conversation already has an active turn."
	msgResyncRequired   = "The event cursor is no longer available; reload the conversation."
	msgInvalidRequest   = "The request is invalid."
	msgPayloadTooLarge  = "The request body is too large."
	msgMethodNotAllowed = "The method is not allowed."
	msgInternal         = "The request could not be completed."

	msgProposalUnsupported = "This proposal cannot be decided here."
	msgProposalStale       = "The proposal is out of date."
	msgProposalDecided     = "The proposal was already decided."
	msgProposalExpired     = "The proposal expired."
	msgArgumentEditsOff    = "This proposal's arguments cannot be edited."
	msgInvalidArguments    = "The arguments are invalid."
	// msgProposalOutcomeUnsupported answers an editor outcome report on a
	// proposal whose effect happens on the server.
	msgProposalOutcomeUnsupported = "This proposal has no editor outcome."
	// msgProposalNotApproved answers a report or decision for a proposal that
	// the user has not approved.
	msgProposalNotApproved = "The proposal has not been approved."
	msgUnsupportedMedia    = "The file type is not supported."
	msgTooManyUploads      = "The conversation has too many uploads."
	msgInvalidAttachment   = "An attachment is invalid."
	msgUnsupportedAttach   = "An attachment type is not supported."
)

const (
	defaultMaxBodyBytes      = 64 << 10
	defaultMaxContextBytes   = 16 << 10
	defaultMaxMessageChars   = 16000
	defaultHeartbeatInterval = 15 * time.Second

	// maxTitleRunes bounds a conversation title on create and rename.
	maxTitleRunes = 200
)

// ErrUnauthorized reports missing or invalid caller credentials. An
// Authenticator returns it (wrapped is fine) for both cases, because the wire
// response must not distinguish them.
var ErrUnauthorized = errors.New("chat: unauthorized")

// ErrForbidden reports an authenticated caller that may not use chat.
var ErrForbidden = errors.New("chat: forbidden")

// Caller is the resolved identity of one request.
type Caller struct {
	// Owner is the full ownership partition every store lookup uses. It must
	// validate.
	Owner OwnerKey
	// Tools is the tool executor bound to this caller. It is handed to the
	// runner unchanged, so the turn can never widen the caller's access.
	Tools ToolExecutor
	// Expires is the caller credential's expiry; zero means unknown.
	Expires time.Time
}

// Authenticator resolves the caller of one request from the request itself, for
// example from its Authorization header. Tokens are only ever read here, never
// from a URL.
//
// It returns ErrUnauthorized for missing or invalid credentials and ErrForbidden
// when the caller is authenticated but not allowed to use chat. Any other error
// is treated as a server-side failure.
type Authenticator interface {
	Authenticate(r *http.Request) (Caller, error)
}

// HandlerConfig configures NewHandler.
type HandlerConfig struct {
	// Prefix is the API root, for example "/api/mcp/chat/v1". It is required,
	// must start with "/" and must not end with "/".
	Prefix string
	// Runner executes turns.
	Runner *Runner
	// Store is the persistence adapter. It must be the store the Runner uses;
	// the handler reads conversations, turns and transcripts from it.
	Store Store
	// Auth authenticates every request.
	Auth Authenticator
	// Router, when set, supplies the model choices and resolves a turn's
	// provider and model. Nil keeps the runner's legacy single-provider path.
	Router ModelRouter
	// Models is the allowlist reported by /status. A turn may only select one
	// of these models; the list is informational otherwise in v1.1. With a
	// Router, an empty list is derived from the router's choices.
	Models []string
	// MaxBodyBytes bounds a JSON request body. Zero means 64 KiB.
	MaxBodyBytes int64
	// MaxMessageChars bounds one user message, counted in runes. Zero means
	// 16000.
	MaxMessageChars int
	// HeartbeatInterval is how often an idle event stream emits a comment
	// line. Zero means 15s.
	HeartbeatInterval time.Duration
	// TurnContext accepts a "context" object on a turn submission: the page
	// the user is on, their selection and the like, which the runner's Setup
	// hook (or, by default, the system prompt) receives. False means a turn
	// with a context is rejected, so a client that never sends one sees the
	// previous surface.
	TurnContext bool
	// MaxContextBytes bounds a turn's context, as sent. Zero means 16 KiB. It
	// counts toward MaxBodyBytes too.
	MaxContextBytes int
	// TargetCheck validates a client-supplied target reference for this
	// caller. Nil means targets are not supported at all: creating a targeted
	// conversation or filtering by target is rejected. It is re-run on every
	// request that uses a stored target, because access can be revoked while a
	// conversation exists.
	TargetCheck func(r *http.Request, c Caller, target string) error
	// Proposals makes the handler serve the proposal endpoints and resolve the
	// conversation's undecided proposals. Nil means proposals are off: the
	// decision routes do not exist here, and /status reports them disabled, so
	// a client that never uses proposals sees exactly the v1.1 surface.
	Proposals ProposalStore
	// AllowArgumentEdits lets a decision replace a pending proposal's
	// arguments before approving it. The executor validates the edit and the
	// store records a new revision, so a decision never addresses arguments
	// the user did not see.
	AllowArgumentEdits bool
	// DecisionTimeout bounds how long applying an approved proposal may take.
	// Zero means 30s. It is independent of the request lifetime: a client that
	// disconnects cannot leave an execution half-recorded.
	DecisionTimeout time.Duration
	// Uploads serves the attachment endpoints and accepts attachment
	// references on a turn. Nil means uploads are off: the upload routes do
	// not exist, /status reports them disabled and a turn body with
	// attachments is rejected, so a client that never uploads sees exactly the
	// previous surface.
	Uploads UploadStore
	// MaxUploadBytes bounds one uploaded file. Zero means 10 MiB.
	MaxUploadBytes int64
	// MaxAttachmentsPerTurn bounds the upload ids one user message may
	// reference. Zero means 5.
	MaxAttachmentsPerTurn int
	// MaxUploadsPerConversation bounds how many uploads one conversation may
	// keep. Zero means 50.
	MaxUploadsPerConversation int
	// ProviderAdmin, when set together with AdminAuth, mounts the superuser
	// provider management routes under the chat prefix. It never widens the
	// normal chat surface.
	ProviderAdmin ProviderAdmin
	// AdminAuth authenticates the provider admin routes. It must be a
	// superuser-only authenticator; the chat auth and policy are not applied to
	// these routes.
	AdminAuth Authenticator
}

// NewHandler validates cfg and returns the chat HTTP handler.
func NewHandler(cfg HandlerConfig) (http.Handler, error) {
	if cfg.Runner == nil {
		return nil, errors.New("chat: handler requires a runner")
	}
	if cfg.Store == nil {
		return nil, errors.New("chat: handler requires a store")
	}
	if cfg.Auth == nil {
		return nil, errors.New("chat: handler requires an authenticator")
	}
	if cfg.ProviderAdmin != nil && cfg.AdminAuth == nil {
		return nil, errors.New("chat: provider admin requires an admin authenticator")
	}
	prefix, err := validatePrefix(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	cfg.Prefix = prefix
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxMessageChars <= 0 {
		cfg.MaxMessageChars = defaultMaxMessageChars
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaultHeartbeatInterval
	}
	if cfg.MaxContextBytes <= 0 {
		cfg.MaxContextBytes = defaultMaxContextBytes
	}
	if cfg.DecisionTimeout <= 0 {
		cfg.DecisionTimeout = defaultDecisionTimeout
	}
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = defaultMaxUploadBytes
	}
	if cfg.MaxAttachmentsPerTurn <= 0 {
		cfg.MaxAttachmentsPerTurn = defaultMaxAttachmentsPerTurn
	}
	if cfg.MaxUploadsPerConversation <= 0 {
		cfg.MaxUploadsPerConversation = defaultMaxUploadsPerConversation
	}
	cfg.Models = normalizeModels(cfg.Models)

	h := &handler{cfg: cfg}
	h.mux = h.routes()
	return h, nil
}

// validatePrefix enforces a clean absolute path, because ServeMux patterns are
// built from it and a malformed prefix would either panic or route wrongly.
func validatePrefix(p string) (string, error) {
	if p == "" {
		return "", errors.New("chat: handler requires a prefix")
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("chat: prefix %q must start with /", p)
	}
	if strings.HasSuffix(p, "/") {
		return "", fmt.Errorf("chat: prefix %q must not end with /", p)
	}
	if path.Clean(p) != p {
		return "", fmt.Errorf("chat: prefix %q is not a clean path", p)
	}
	if strings.ContainsAny(p, "?#{} \t\r\n") {
		return "", fmt.Errorf("chat: prefix %q contains an unsupported character", p)
	}
	return p, nil
}

// normalizeModels trims the allowlist and drops empty duplicates, so /status
// and model validation agree on one canonical list.
func normalizeModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// handler serves the chat API. Every request is authenticated and every
// response is non-cacheable; only the mux decides which endpoint runs.
type handler struct {
	cfg HandlerConfig
	mux *http.ServeMux
}

type callerContextKey struct{}

// ServeHTTP authenticates the request before routing it, because even a
// method-not-allowed or not-found response must not be available to an
// anonymous caller.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if hasTokenQuery(r) {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}

	// Provider admin routes get their own authenticator. A superuser may be
	// rejected by the user-facing chat gate (Auth.AllowSuperusers off) and still
	// have to reach the settings surface, and the chat policy must not apply
	// there.
	auth := h.cfg.Auth
	if h.providerAdminPath(r.URL.Path) {
		auth = h.cfg.AdminAuth
	}
	caller, err := auth.Authenticate(r)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnauthorized):
			w.Header().Set("WWW-Authenticate", "Bearer")
			h.writeError(w, http.StatusUnauthorized, CodeUnauthorized, msgUnauthorized)
		case errors.Is(err, ErrForbidden):
			h.writeError(w, http.StatusForbidden, CodeForbidden, msgForbidden)
		default:
			// An authenticator failure is a server problem. Its text may name
			// internals, so it is replaced with the generic message.
			h.writeError(w, http.StatusInternalServerError, CodeInternal, msgInternal)
		}
		return
	}
	if err := caller.Owner.Validate(); err != nil {
		// A nil error with an incomplete owner is a wiring bug, not a caller
		// error. Refuse rather than run a turn in a partition nobody owns.
		h.writeError(w, http.StatusInternalServerError, CodeInternal, msgInternal)
		return
	}

	ctx := context.WithValue(r.Context(), callerContextKey{}, caller)
	h.mux.ServeHTTP(w, r.WithContext(ctx))
}

// routeShape is one registered method and path pattern.
type routeShape struct {
	method string
	path   string
}

// adminMethodFallback answers a provider admin path whose method has no route:
// 405 with an Allow header when some route matches the path, otherwise the
// JSON not-found body.
func (h *handler) adminMethodFallback(routes []routeShape) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var allow []string
		seen := map[string]bool{}
		for _, rt := range routes {
			if !routePathMatches(rt.path, r.URL.Path) || seen[rt.method] {
				continue
			}
			seen[rt.method] = true
			allow = append(allow, rt.method)
			if rt.method == http.MethodGet && !seen[http.MethodHead] {
				seen[http.MethodHead] = true
				allow = append(allow, http.MethodHead)
			}
		}
		if len(allow) == 0 {
			h.notFound(w, r)
			return
		}
		h.methodNotAllowed(strings.Join(allow, ", "))(w, r)
	}
}

// routePathMatches reports whether path fits pattern, where a "{name}" segment
// matches any one non-empty segment.
func routePathMatches(pattern, path string) bool {
	want := strings.Split(pattern, "/")
	got := strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i, seg := range want {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			if got[i] == "" {
				return false
			}
			continue
		}
		if seg != got[i] {
			return false
		}
	}
	return true
}

// providerAdminPath reports whether path belongs to the provider admin
// surface. It is only consulted when that surface is mounted, so an ordinary
// chat path is never routed to a nil AdminAuth.
func (h *handler) providerAdminPath(p string) bool {
	if h.cfg.ProviderAdmin == nil {
		return false
	}
	base := h.cfg.Prefix + "/providers"
	return p == base || strings.HasPrefix(p, base+"/")
}

// hasTokenQuery reports whether a credential was smuggled into the URL. Tokens
// belong in headers only: URLs leak into logs, history and referrers.
func hasTokenQuery(r *http.Request) bool {
	q := r.URL.Query()
	for _, name := range []string{"token", "access_token", "auth"} {
		if q.Has(name) {
			return true
		}
	}
	return false
}

// routes builds the method-and-path table. Each path also gets a method-less
// fallback so an unsupported method answers with the protocol's JSON 405
// instead of ServeMux's plain-text one; the method patterns stay more specific,
// so they always win.
func (h *handler) routes() *http.ServeMux {
	p := h.cfg.Prefix
	type route struct {
		method string
		path   string
		fn     http.HandlerFunc
	}
	table := []route{
		{http.MethodGet, p + "/status", h.serveStatus},
		{http.MethodGet, p + "/conversations", h.listConversations},
		{http.MethodPost, p + "/conversations", h.createConversation},
		{http.MethodGet, p + "/conversations/{id}", h.getConversation},
		{http.MethodPatch, p + "/conversations/{id}", h.renameConversation},
		{http.MethodDelete, p + "/conversations/{id}", h.deleteConversation},
		{http.MethodPost, p + "/conversations/{id}/turns", h.submitTurn},
		{http.MethodGet, p + "/turns/{id}", h.getTurn},
		{http.MethodGet, p + "/turns/{id}/events", h.turnEvents},
		{http.MethodPost, p + "/turns/{id}/cancel", h.cancelTurn},
	}
	if h.cfg.Proposals != nil {
		// The proposal endpoints only exist when a host can store proposals, so
		// a read-only deployment answers them exactly like any unknown route.
		table = append(table,
			route{http.MethodGet, p + "/proposals/{id}", h.getProposal},
			route{http.MethodPost, p + "/proposals/{id}/decision", h.decideProposal},
			route{http.MethodPost, p + "/proposals/{id}/outcome", h.proposalOutcome},
		)
	}
	if h.cfg.Uploads != nil {
		// Upload routes exist only when a host can store uploads, so a
		// deployment without them answers exactly like any unknown route.
		table = append(table,
			route{http.MethodPost, p + "/uploads", h.createUpload},
			route{http.MethodGet, p + "/uploads/{id}", h.downloadUpload},
			route{http.MethodGet, p + "/uploads/{id}/meta", h.uploadMeta},
		)
	}
	if h.cfg.ProviderAdmin != nil {
		// Superuser-only provider management. AdminAuth gates these paths in
		// ServeHTTP before the normal chat authenticator runs.
		table = append(table,
			route{http.MethodGet, p + "/providers", h.listProviders},
			route{http.MethodPost, p + "/providers", h.createProvider},
			// Two-segment action paths never collide with /providers/{id}.
			route{http.MethodPost, p + "/providers/actions/test", h.testProviderConfig},
			route{http.MethodPost, p + "/providers/actions/default", h.setDefaultProvider},
			route{http.MethodPost, p + "/providers/actions/title", h.setTitleChoice},
			route{http.MethodGet, p + "/providers/{id}", h.getProvider},
			route{http.MethodPatch, p + "/providers/{id}", h.updateProvider},
			route{http.MethodDelete, p + "/providers/{id}", h.deleteProvider},
			route{http.MethodPost, p + "/providers/{id}/test", h.testProvider},
			route{http.MethodPost, p + "/providers/{id}/models/refresh", h.refreshProviderModels},
			route{http.MethodPost, p + "/providers/{id}/signin", h.beginProviderSignIn},
			route{http.MethodPost, p + "/providers/{id}/signin/complete", h.completeProviderSignIn},
			route{http.MethodPost, p + "/providers/{id}/signin/cancel", h.cancelProviderSignIn},
			route{http.MethodPost, p + "/providers/{id}/signout", h.signOutProvider},
			route{http.MethodPost, p + "/providers/{id}/import", h.importProviderCredentials},
		)
	}

	mux := http.NewServeMux()
	byPath := make(map[string][]string, len(table))
	var order []string
	var adminRoutes []routeShape
	for _, rt := range table {
		mux.HandleFunc(rt.method+" "+rt.path, rt.fn)
		// Admin routes can't each get a method-less 405 pattern: a literal
		// action path and a wildcard route would conflict in ServeMux. One
		// subtree fallback below answers 405 for them instead.
		if strings.HasPrefix(rt.path, p+"/providers") {
			adminRoutes = append(adminRoutes, routeShape{method: rt.method, path: rt.path})
			continue
		}
		if _, ok := byPath[rt.path]; !ok {
			order = append(order, rt.path)
		}
		byPath[rt.path] = append(byPath[rt.path], rt.method)
	}
	for _, routePath := range order {
		allow := make([]string, 0, len(byPath[routePath])+1)
		allow = append(allow, byPath[routePath]...)
		for _, m := range byPath[routePath] {
			if m == http.MethodGet {
				allow = append(allow, http.MethodHead)
				break
			}
		}
		mux.HandleFunc(routePath, h.methodNotAllowed(strings.Join(allow, ", ")))
	}
	if len(adminRoutes) > 0 {
		// Less specific than every admin route, so it only runs when no
		// method-specific pattern matched.
		fallback := h.adminMethodFallback(adminRoutes)
		mux.HandleFunc(p+"/providers", fallback)
		mux.HandleFunc(p+"/providers/", fallback)
	}

	// Unknown paths under the prefix answer with the protocol's JSON not-found
	// body instead of ServeMux's plain text, and registering the bare prefix
	// avoids a redirect to the trailing-slash subtree.
	mux.HandleFunc(p, h.notFound)
	mux.HandleFunc(p+"/", h.notFound)
	return mux
}

func (h *handler) methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		h.writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, msgMethodNotAllowed)
	}
}

func (h *handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.writeError(w, http.StatusNotFound, CodeNotFound, msgNotFound)
}

// --- responses ---

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (h *handler) writeError(w http.ResponseWriter, status int, code, message string) {
	h.writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

func (h *handler) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Only fixed, marshalable payloads reach here; keep the response
		// well-formed even so.
		body = []byte(`{"error":{"code":"internal","message":"` + msgInternal + `"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// storeError maps a store failure onto the wire. Every not-found case uses one
// body, so another owner's record is indistinguishable from a missing one.
func (h *handler) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		h.writeError(w, http.StatusNotFound, CodeNotFound, msgNotFound)
	case errors.Is(err, ErrActiveTurn):
		h.writeError(w, http.StatusConflict, CodeActiveTurn, msgActiveTurn)
	case errors.Is(err, ErrResyncRequired):
		h.writeError(w, http.StatusConflict, CodeResyncRequired, msgResyncRequired)
	default:
		h.writeError(w, http.StatusInternalServerError, CodeInternal, msgInternal)
	}
}

// listError maps a list failure. The only caller-controlled inputs of a list
// are its page cursor and limit, and adapters report a malformed cursor with an
// ordinary error, so a non-sentinel failure here is a client error.
func (h *handler) listError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		h.writeError(w, http.StatusNotFound, CodeNotFound, msgNotFound)
	case errors.Is(err, ErrResyncRequired):
		h.writeError(w, http.StatusConflict, CodeResyncRequired, msgResyncRequired)
	default:
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
	}
}

// --- request helpers ---

func callerFrom(r *http.Request) (Caller, bool) {
	c, ok := r.Context().Value(callerContextKey{}).(Caller)
	return c, ok
}

func (h *handler) requireCaller(w http.ResponseWriter, r *http.Request) (Caller, bool) {
	caller, ok := callerFrom(r)
	if !ok || caller.Owner.Validate() != nil {
		h.writeError(w, http.StatusInternalServerError, CodeInternal, msgInternal)
		return Caller{}, false
	}
	return caller, true
}

// decodeBody reads one bounded JSON document into dst. Unknown fields and
// trailing data are rejected so a typo can never be silently ignored.
func (h *handler) decodeBody(w http.ResponseWriter, r *http.Request, allowEmpty bool, dst any) bool {
	if r.Body == nil {
		if allowEmpty {
			return true
		}
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, msgPayloadTooLarge)
			return false
		}
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if allowEmpty {
			return true
		}
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return false
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return false
	}
	return true
}

// queryLimit parses an optional page limit. A missing or empty value means the
// store's default.
func (h *handler) queryLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return 0, false
	}
	return n, true
}

// queryPage collects the page parameters shared by every list endpoint.
func (h *handler) queryPage(w http.ResponseWriter, r *http.Request) (Page, bool) {
	limit, ok := h.queryLimit(w, r)
	if !ok {
		return Page{}, false
	}
	return Page{Limit: limit, Cursor: r.URL.Query().Get("cursor")}, true
}

// targetAllowed rechecks a stored target for this request. A target that cannot
// be validated is indistinguishable from a missing record, so a caller cannot
// probe for existence through a revoked reference.
func (h *handler) targetAllowed(r *http.Request, caller Caller, target string) bool {
	if target == "" {
		return true
	}
	if h.cfg.TargetCheck == nil {
		return false
	}
	return h.cfg.TargetCheck(r, caller, target) == nil
}

func (h *handler) modelAllowed(model string) bool {
	for _, m := range h.cfg.Models {
		if m == model {
			return true
		}
	}
	return false
}

// gateConversation loads one caller-owned conversation and rechecks its target.
func (h *handler) gateConversation(w http.ResponseWriter, r *http.Request, caller Caller, id string) (Conversation, bool) {
	conv, err := h.cfg.Store.GetConversation(r.Context(), caller.Owner, id)
	if err != nil {
		h.storeError(w, err)
		return Conversation{}, false
	}
	if !h.targetAllowed(r, caller, conv.Target) {
		h.writeError(w, http.StatusNotFound, CodeNotFound, msgNotFound)
		return Conversation{}, false
	}
	return conv, true
}

// gateTurn loads one caller-owned turn and the conversation it belongs to, then
// rechecks the conversation's target.
func (h *handler) gateTurn(w http.ResponseWriter, r *http.Request, caller Caller, id string) (Turn, Conversation, bool) {
	turn, err := h.cfg.Store.GetTurn(r.Context(), caller.Owner, id)
	if err != nil {
		h.storeError(w, err)
		return Turn{}, Conversation{}, false
	}
	conv, ok := h.gateConversation(w, r, caller, turn.ConversationID)
	if !ok {
		return Turn{}, Conversation{}, false
	}
	return turn, conv, true
}

// activeTurn returns the conversation's non-terminal turn, if any.
//
// A conversation allows at most one active turn, and turns are created in order,
// so the newest turn is the only candidate. The store port has no "list turns"
// operation, but every turn stores at least its user message, which names the
// turn; the transcript therefore identifies the candidate without new state.
func (h *handler) activeTurn(ctx context.Context, owner OwnerKey, conversationID string) (*Turn, error) {
	if fast, ok := h.cfg.Store.(ActiveTurnStore); ok {
		turn, found, err := fast.ActiveTurn(ctx, owner, conversationID)
		if err != nil || !found {
			return nil, err
		}
		return &turn, nil
	}
	// Fallback for stores without the fast path: the newest turn referenced by
	// the transcript.
	var lastID string
	page := Page{Limit: maxPageLimit}
	for {
		msgs, next, err := h.cfg.Store.ListMessages(ctx, owner, conversationID, page)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if m.TurnID != "" {
				lastID = m.TurnID
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if lastID == "" {
		return nil, nil
	}
	turn, err := h.cfg.Store.GetTurn(ctx, owner, lastID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if turn.State.Terminal() {
		return nil, nil
	}
	return &turn, nil
}

// --- response shapes ---

type featuresView struct {
	Proposals bool `json:"proposals"`
	Uploads   bool `json:"uploads"`
	// Context is only present when turns accept a page context.
	Context bool `json:"context,omitempty"`
}

type limitsView struct {
	MaxMessageChars int `json:"maxMessageChars"`
	// The upload limits and attachment types are only present when uploads
	// are enabled, so a deployment without them emits exactly the previous
	// body. AttachmentTypes is a pointer so an enabled surface whose provider
	// supports none reports an empty list instead of omitting the field.
	MaxUploadBytes        int64     `json:"maxUploadBytes,omitempty"`
	MaxAttachmentsPerTurn int       `json:"maxAttachmentsPerTurn,omitempty"`
	AttachmentTypes       *[]string `json:"attachmentTypes,omitempty"`
	// MaxContextBytes is only present when turns accept a page context.
	MaxContextBytes int `json:"maxContextBytes,omitempty"`
}

type statusView struct {
	Version string   `json:"version"`
	Models  []string `json:"models"`
	// Choices and DefaultChoice are present when a model router is configured.
	// Models stays a flat de-duplicated list for older clients.
	Choices       []ModelChoice `json:"choices,omitempty"`
	DefaultChoice string        `json:"defaultChoice,omitempty"`
	Features      featuresView  `json:"features"`
	Limits        limitsView    `json:"limits"`
}

type conversationListView struct {
	Items      []Conversation `json:"items"`
	NextCursor string         `json:"nextCursor"`
}

type conversationDetailView struct {
	Conversation Conversation `json:"conversation"`
	Messages     []Message    `json:"messages"`
	NextCursor   string       `json:"nextCursor"`
	ActiveTurn   *Turn        `json:"activeTurn"`
}

// conversationDetailWithProposalsView is conversationDetailView plus the
// conversation's undecided proposals. It is a separate type so a deployment
// without proposals emits exactly the v1.1 body, with no extra field.
type conversationDetailWithProposalsView struct {
	Conversation Conversation `json:"conversation"`
	Messages     []Message    `json:"messages"`
	NextCursor   string       `json:"nextCursor"`
	ActiveTurn   *Turn        `json:"activeTurn"`
	// Proposals lists pending, approved and executing proposals, oldest first.
	Proposals []Proposal `json:"proposals"`
}

type submitTurnView struct {
	Turn    Turn `json:"turn"`
	Created bool `json:"created"`
}

type createConversationRequest struct {
	Title  *string `json:"title"`
	Target *string `json:"target"`
}

type renameConversationRequest struct {
	Title *string `json:"title"`
}

type submitTurnRequest struct {
	Text        *string         `json:"text"`
	Provider    *string         `json:"provider"`
	Model       *string         `json:"model"`
	Effort      *string         `json:"effort"`
	Attachments []string        `json:"attachments"`
	Context     json.RawMessage `json:"context"`
}

// uploadView is the response body of every upload endpoint. The bytes are
// served only by the download route, never embedded in JSON.
type uploadView struct {
	Upload Upload `json:"upload"`
}

// --- uploads ---

// createUpload accepts one multipart attachment. The body is bounded before
// parsing, so an oversized file is refused without being held in memory, and the
// type is assigned from the bytes rather than from the client's declaration.
func (h *handler) createUpload(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxUploadBytes+maxMultipartOverhead)
	reader, err := r.MultipartReader()
	if err != nil {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}

	var (
		conversationID string
		filename       string
		data           []byte
		sawFile        bool
	)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if h.tooLarge(w, err) {
				return
			}
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
			return
		}
		switch part.FormName() {
		case "conversationId":
			raw, err := io.ReadAll(io.LimitReader(part, 1024))
			_ = part.Close()
			if err != nil {
				if h.tooLarge(w, err) {
					return
				}
				h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
				return
			}
			conversationID = strings.TrimSpace(string(raw))
		case "file":
			if sawFile {
				_ = part.Close()
				h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
				return
			}
			sawFile = true
			filename = part.FileName()
			// Reading one byte past the limit distinguishes an oversized file
			// from one that exactly fills it, without trimming a valid upload.
			data, err = io.ReadAll(io.LimitReader(part, h.cfg.MaxUploadBytes+1))
			_ = part.Close()
			if err != nil {
				if h.tooLarge(w, err) {
					return
				}
				h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
				return
			}
			if int64(len(data)) > h.cfg.MaxUploadBytes {
				h.writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, msgPayloadTooLarge)
				return
			}
		default:
			// Unknown form fields carry no meaning here; skipping them keeps a
			// normal browser form working.
			_ = part.Close()
		}
	}
	if conversationID == "" || !sawFile {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}

	conv, ok := h.gateConversation(w, r, caller, conversationID)
	if !ok {
		return
	}
	count, err := h.cfg.Uploads.CountUploads(r.Context(), caller.Owner, conv.ID)
	if err != nil {
		h.storeError(w, err)
		return
	}
	if count >= h.cfg.MaxUploadsPerConversation {
		h.writeError(w, http.StatusConflict, CodeTooManyUploads, msgTooManyUploads)
		return
	}

	name := SanitizeUploadName(filename)
	mimeType, kind, ok := DetectUpload(name, data)
	if !ok {
		h.writeError(w, http.StatusUnsupportedMediaType, CodeUnsupportedMedia, msgUnsupportedMedia)
		return
	}
	stored, err := h.cfg.Uploads.CreateUpload(r.Context(), caller.Owner, Upload{
		ConversationID: conv.ID,
		Name:           name,
		MIME:           mimeType,
		Kind:           kind,
	}, data)
	if err != nil {
		h.storeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, uploadView{Upload: stored})
}

// tooLarge maps a bounded-body failure onto 413 and reports whether it was one.
func (h *handler) tooLarge(w http.ResponseWriter, err error) bool {
	var max *http.MaxBytesError
	if !errors.As(err, &max) {
		return false
	}
	h.writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, msgPayloadTooLarge)
	return true
}

// downloadUpload streams one caller-owned attachment with explicit download
// headers. A browser must never render an upload inline: the response is
// always an opaque attachment, sandboxed and non-cacheable.
func (h *handler) downloadUpload(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	upload, data, err := h.cfg.Uploads.ReadUpload(r.Context(), caller.Owner, r.PathValue("id"))
	if err != nil {
		h.storeError(w, err)
		return
	}
	if _, ok := h.gateConversation(w, r, caller, upload.ConversationID); !ok {
		return
	}
	w.Header().Set("Content-Type", upload.MIME)
	w.Header().Set("Content-Disposition", contentDisposition(upload.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	// ServeHTTP sets no-store; the stricter private form is what an
	// authenticated download requires.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// uploadMeta returns one caller-owned attachment's metadata.
func (h *handler) uploadMeta(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	upload, err := h.cfg.Uploads.GetUpload(r.Context(), caller.Owner, r.PathValue("id"))
	if err != nil {
		h.storeError(w, err)
		return
	}
	if _, ok := h.gateConversation(w, r, caller, upload.ConversationID); !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, uploadView{Upload: upload})
}

// contentDisposition renders an attachment disposition with an RFC 5987
// filename. Only the extended form is emitted, so a non-ASCII name never has to
// be squeezed into a quoted string.
func contentDisposition(name string) string {
	return "attachment; filename*=UTF-8''" + rfc5987Encode(name)
}

// rfc5987Encode percent-encodes every byte outside attr-char.
func rfc5987Encode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isAttrChar(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

func isAttrChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// turnAttachments validates the attachment ids of one submission. Every id must
// be the caller's upload in this conversation, and its type must be one the
// provider can send. A missing, foreign or duplicated id is the same 400, so a
// caller cannot probe for uploads they do not own.
func (h *handler) turnAttachments(w http.ResponseWriter, r *http.Request, caller Caller, conversationID string, ids []string, target *ResolvedModel) ([]Upload, bool) {
	if h.cfg.Uploads == nil {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return nil, false
	}
	if len(ids) > h.cfg.MaxAttachmentsPerTurn {
		h.writeError(w, http.StatusBadRequest, CodeInvalidAttachment, msgInvalidAttachment)
		return nil, false
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]Upload, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			h.writeError(w, http.StatusBadRequest, CodeInvalidAttachment, msgInvalidAttachment)
			return nil, false
		}
		if _, dup := seen[id]; dup {
			h.writeError(w, http.StatusBadRequest, CodeInvalidAttachment, msgInvalidAttachment)
			return nil, false
		}
		seen[id] = struct{}{}
		upload, err := h.cfg.Uploads.GetUpload(r.Context(), caller.Owner, id)
		if err != nil || upload.ConversationID != conversationID {
			h.writeError(w, http.StatusBadRequest, CodeInvalidAttachment, msgInvalidAttachment)
			return nil, false
		}
		out = append(out, upload)
	}
	// Support is checked only after every reference resolved, so an invalid id
	// is never distinguishable from an unsupported one. A resolved target is
	// checked against that provider; otherwise the runner's configured provider
	// (or the catalog union) answers.
	for _, upload := range out {
		supported := h.cfg.Runner.SupportsAttachment(upload.MIME)
		if target != nil {
			support, ok := target.Provider.(provider.AttachmentSupport)
			supported = ok && support.SupportsAttachment(upload.MIME)
		}
		if !supported {
			h.writeError(w, http.StatusBadRequest, CodeUnsupportedAttachment, msgUnsupportedAttach)
			return nil, false
		}
	}
	return out, true
}

// --- endpoints ---

func (h *handler) serveStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireCaller(w, r); !ok {
		return
	}
	models := append([]string{}, h.cfg.Models...)
	var choices []ModelChoice
	defaultChoice := ModelChoice{}
	if h.cfg.Router != nil {
		var err error
		choices, err = h.cfg.Router.Choices(r.Context())
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, CodeInternal, msgInternal)
			return
		}
		defaultChoice, _ = h.cfg.Router.Default(r.Context())
		for i := range choices {
			choices[i].Default = choices[i].ID == defaultChoice.ID
		}
		if len(models) == 0 {
			models = modelIDs(choices)
		}
	}
	limits := limitsView{MaxMessageChars: h.cfg.MaxMessageChars}
	if h.cfg.TurnContext {
		limits.MaxContextBytes = h.cfg.MaxContextBytes
	}
	if h.cfg.Uploads != nil {
		limits.MaxUploadBytes = h.cfg.MaxUploadBytes
		limits.MaxAttachmentsPerTurn = h.cfg.MaxAttachmentsPerTurn
		// The same set POST /uploads accepts: with a router, the union over
		// ready providers. Each choice lists what it can send, and a turn is
		// checked against its resolved provider.
		types := attachmentTypesFor(h.cfg.Runner.SupportsAttachment)
		limits.AttachmentTypes = &types
	}
	h.writeJSON(w, http.StatusOK, statusView{
		Version:       ProtocolVersion,
		Models:        models,
		Choices:       choices,
		DefaultChoice: defaultChoice.ID,
		Features: featuresView{
			Proposals: h.cfg.Proposals != nil,
			Uploads:   h.cfg.Uploads != nil,
			Context:   h.cfg.TurnContext,
		},
		Limits: limits,
	})
}

// modelIDs flattens choices into the legacy de-duplicated model list.
func modelIDs(choices []ModelChoice) []string {
	out := make([]string, 0, len(choices))
	seen := make(map[string]struct{}, len(choices))
	for _, choice := range choices {
		if choice.Model == "" {
			continue
		}
		if _, ok := seen[choice.Model]; ok {
			continue
		}
		seen[choice.Model] = struct{}{}
		out = append(out, choice.Model)
	}
	return out
}

func (h *handler) listConversations(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	page, ok := h.queryPage(w, r)
	if !ok {
		return
	}
	target := r.URL.Query().Get("target")
	if target != "" {
		if h.cfg.TargetCheck == nil {
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
			return
		}
		if err := h.cfg.TargetCheck(r, caller, target); err != nil {
			h.writeError(w, http.StatusForbidden, CodeForbidden, msgForbidden)
			return
		}
	}

	items, next, err := h.cfg.Store.ListConversations(r.Context(), caller.Owner, target, page)
	if err != nil {
		h.listError(w, err)
		return
	}
	out := make([]Conversation, 0, len(items))
	for _, c := range items {
		if c.Target != "" && c.Target != target && !h.targetAllowed(r, caller, c.Target) {
			continue
		}
		out = append(out, c)
	}
	h.writeJSON(w, http.StatusOK, conversationListView{Items: out, NextCursor: next})
}

func (h *handler) createConversation(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	var req createConversationRequest
	if !h.decodeBody(w, r, true, &req) {
		return
	}
	title := ""
	if req.Title != nil {
		title = strings.TrimSpace(*req.Title)
	}
	if utf8.RuneCountInString(title) > maxTitleRunes {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}
	target := ""
	if req.Target != nil {
		target = *req.Target
	}
	if target != "" {
		if h.cfg.TargetCheck == nil {
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
			return
		}
		if err := h.cfg.TargetCheck(r, caller, target); err != nil {
			h.writeError(w, http.StatusForbidden, CodeForbidden, msgForbidden)
			return
		}
	}

	conv, err := h.cfg.Store.CreateConversation(r.Context(), Conversation{
		Owner:  caller.Owner,
		Title:  title,
		Target: target,
	})
	if err != nil {
		h.storeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, conv)
}

func (h *handler) getConversation(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	conv, ok := h.gateConversation(w, r, caller, r.PathValue("id"))
	if !ok {
		return
	}
	page, ok := h.queryPage(w, r)
	if !ok {
		return
	}
	msgs, next, err := h.cfg.Store.ListMessages(r.Context(), caller.Owner, conv.ID, page)
	if err != nil {
		h.listError(w, err)
		return
	}
	if msgs == nil {
		msgs = []Message{}
	}
	active, err := h.activeTurn(r.Context(), caller.Owner, conv.ID)
	if err != nil {
		h.storeError(w, err)
		return
	}
	if h.cfg.Proposals == nil {
		h.writeJSON(w, http.StatusOK, conversationDetailView{
			Conversation: conv,
			Messages:     msgs,
			NextCursor:   next,
			ActiveTurn:   active,
		})
		return
	}
	// The client needs the proposals it can still decide, because no event
	// stream carries one created by an earlier, finished turn.
	proposals, err := h.nonTerminalProposals(r.Context(), caller.Owner, conv.ID)
	if err != nil {
		h.storeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, conversationDetailWithProposalsView{
		Conversation: conv,
		Messages:     msgs,
		NextCursor:   next,
		ActiveTurn:   active,
		Proposals:    proposals,
	})
}

func (h *handler) renameConversation(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	if _, ok := h.gateConversation(w, r, caller, r.PathValue("id")); !ok {
		return
	}
	var req renameConversationRequest
	if !h.decodeBody(w, r, false, &req) {
		return
	}
	if req.Title == nil {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}
	title := strings.TrimSpace(*req.Title)
	if utf8.RuneCountInString(title) > maxTitleRunes {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}

	conv, err := h.cfg.Store.RenameConversation(r.Context(), caller.Owner, r.PathValue("id"), title)
	if err != nil {
		h.storeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, conv)
}

// deleteConversation cancels active work first, because the store refuses to
// delete a conversation that still owns a non-terminal turn. A turn that starts
// in the race window is cancelled by the retry.
func (h *handler) deleteConversation(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, ok := h.gateConversation(w, r, caller, id); !ok {
		return
	}

	for attempt := 0; attempt < 2; attempt++ {
		active, err := h.activeTurn(r.Context(), caller.Owner, id)
		if err != nil {
			h.storeError(w, err)
			return
		}
		if active != nil {
			if _, err := h.cfg.Runner.Cancel(r.Context(), caller.Owner, active.ID); err != nil && !errors.Is(err, ErrNotFound) {
				h.storeError(w, err)
				return
			}
		}
		err = h.cfg.Store.DeleteConversation(r.Context(), caller.Owner, id)
		if err == nil {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !errors.Is(err, ErrActiveTurn) {
			h.storeError(w, err)
			return
		}
	}
	h.writeError(w, http.StatusConflict, CodeActiveTurn, msgActiveTurn)
}

func (h *handler) submitTurn(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	conv, ok := h.gateConversation(w, r, caller, r.PathValue("id"))
	if !ok {
		return
	}
	var req submitTurnRequest
	if !h.decodeBody(w, r, false, &req) {
		return
	}
	if req.Text == nil {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}
	text := strings.TrimSpace(*req.Text)
	if text == "" || utf8.RuneCountInString(text) > h.cfg.MaxMessageChars {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}
	turnContext, ok := h.turnContext(w, req.Context)
	if !ok {
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	// Return a previously created turn before resolving a provider or
	// validating attachments. A retry after a lost response must not fail
	// because the catalog changed in the meantime.
	if idempotencyKey != "" {
		if finder, ok := h.cfg.Store.(IdempotentTurnStore); ok {
			turn, found, err := finder.FindTurnByIdempotencyKey(r.Context(), caller.Owner, conv.ID, idempotencyKey)
			if err != nil {
				h.storeError(w, err)
				return
			}
			if found {
				h.writeJSON(w, http.StatusAccepted, submitTurnView{Turn: turn, Created: false})
				return
			}
		}
	}
	provider := ""
	if req.Provider != nil {
		provider = strings.TrimSpace(*req.Provider)
	}
	model := ""
	if req.Model != nil {
		model = strings.TrimSpace(*req.Model)
	}
	effort := ""
	if req.Effort != nil {
		effort = strings.TrimSpace(*req.Effort)
	}

	var target *ResolvedModel
	if h.cfg.Router != nil {
		resolved, err := h.cfg.Router.Resolve(r.Context(), provider, model)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
			return
		}
		target = &resolved
		provider = resolved.Choice.Provider
		model = resolved.Model
		if effort != "" && !slices.Contains(resolved.Choice.Efforts, effort) {
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
			return
		}
	} else {
		if provider != "" {
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
			return
		}
		if model != "" && !h.modelAllowed(model) {
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
			return
		}
		// The legacy single provider advertises no efforts.
		if effort != "" {
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
			return
		}
	}
	var attachments []Upload
	if len(req.Attachments) > 0 {
		var ok bool
		attachments, ok = h.turnAttachments(w, r, caller, conv.ID, req.Attachments, target)
		if !ok {
			return
		}
	}

	// The turn outlives the submission request, so a disconnect must not leave
	// a claimed turn half-written. The worker still runs on its own lifetime;
	// this only protects the claim itself.
	ctx := context.WithoutCancel(r.Context())
	turn, created, err := h.cfg.Runner.SubmitWithOptions(ctx, caller.Owner, conv.ID, SubmitOptions{
		Text:           text,
		Attachments:    attachments,
		IdempotencyKey: idempotencyKey,
		Provider:       provider,
		Model:          model,
		Effort:         effort,
		Target:         target,
		Tools:          caller.Tools,
		Deadline:       caller.Expires,
		Context:        turnContext,
		// gateConversation checked the caller may still use the target.
		ConversationTarget: conv.Target,
	})
	if err != nil {
		var refused *TurnRefusedError
		if errors.As(err, &refused) {
			h.writeError(w, http.StatusBadRequest, CodeTurnRefused, refused.Message)
			return
		}
		if errors.Is(err, ErrModelUnknown) || errors.Is(err, ErrModelAmbiguous) || errors.Is(err, ErrNoModelConfigured) || errors.Is(err, ErrEffortUnsupported) {
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
			return
		}
		h.storeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusAccepted, submitTurnView{Turn: turn, Created: created})
}

// turnContext checks a submitted page context: off unless the host turned it
// on, a JSON object, and within MaxContextBytes. An absent or null context is
// nil.
func (h *handler) turnContext(w http.ResponseWriter, raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, true
	}
	if !h.cfg.TurnContext || trimmed[0] != '{' {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return nil, false
	}
	if len(trimmed) > h.cfg.MaxContextBytes {
		h.writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, msgPayloadTooLarge)
		return nil, false
	}
	return append(json.RawMessage(nil), trimmed...), true
}

func (h *handler) getTurn(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	turn, _, ok := h.gateTurn(w, r, caller, r.PathValue("id"))
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, turn)
}

func (h *handler) cancelTurn(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	turn, _, ok := h.gateTurn(w, r, caller, r.PathValue("id"))
	if !ok {
		return
	}
	// Cancelling a terminal turn is a no-op that returns the stored turn, so
	// this is safe to call unconditionally.
	after, err := h.cfg.Runner.Cancel(r.Context(), caller.Owner, turn.ID)
	if err != nil {
		h.storeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, after)
}

// resumeAfter resolves the replay point: the larger of the query cursor and the
// SSE Last-Event-ID header. Both are optional and default to 0.
func (h *handler) resumeAfter(w http.ResponseWriter, r *http.Request) (int64, bool) {
	var after int64
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
			return 0, false
		}
		after = n
	}
	// A malformed header is ignored rather than fatal: replaying from the
	// earlier point is always safe, and a proxy may rewrite the header.
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > after {
			after = n
		}
	}
	return after, true
}

// turnEvents streams one turn's events as Server-Sent Events.
//
// The request context bounds only this subscription: losing the client detaches
// the reader and never cancels the turn. The subscription is registered before
// any byte is written, so a resync-required answer is still a clean JSON
// response.
func (h *handler) turnEvents(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.requireCaller(w, r)
	if !ok {
		return
	}
	turn, _, ok := h.gateTurn(w, r, caller, r.PathValue("id"))
	if !ok {
		return
	}
	after, ok := h.resumeAfter(w, r)
	if !ok {
		return
	}

	events, err := h.cfg.Runner.Subscribe(r.Context(), caller.Owner, turn.ID, after)
	if err != nil {
		switch {
		case errors.Is(err, ErrResyncRequired):
			h.writeError(w, http.StatusConflict, CodeResyncRequired, msgResyncRequired)
		case errors.Is(err, ErrNotFound):
			h.writeError(w, http.StatusNotFound, CodeNotFound, msgNotFound)
		default:
			h.writeError(w, http.StatusInternalServerError, CodeInternal, msgInternal)
		}
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	// A writer that cannot flush (some wrappers drop the interface) is not
	// fatal; the stream then arrives in larger pieces. Any other flush failure
	// means the connection is gone.
	flush := func() bool {
		err := rc.Flush()
		return err == nil || errors.Is(err, http.ErrNotSupported)
	}
	if !flush() {
		return
	}

	heartbeat := time.NewTicker(h.cfg.HeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case e, open := <-events:
			if !open {
				// A gap or a slow consumer ended the stream. The client
				// reconnects with its last sequence and replays the store.
				return
			}
			if err := e.WriteSSE(w); err != nil {
				return
			}
			if !flush() {
				return
			}
			if e.Type.Terminal() {
				return
			}
		case <-heartbeat.C:
			if err := WriteHeartbeat(w); err != nil {
				return
			}
			if !flush() {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}
