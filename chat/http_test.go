package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/scripted"
)

const (
	testPrefix = "/api/mcp/chat/v1"

	tokenA         = "Bearer user-a"
	tokenB         = "Bearer user-b"
	tokenExpired   = "Bearer expired"
	tokenForbidden = "Bearer forbidden"
	tokenBroken    = "Bearer broken"
)

// ownerFor is the ownership partition the fake authenticator binds a token to.
func ownerFor(user string) OwnerKey {
	return OwnerKey{
		Namespace:        "test-ns",
		Mount:            "test-mount",
		AuthCollectionID: "users",
		UserID:           user,
	}
}

// fakeAuth authenticates from the Authorization header only, so a test can
// prove no token is ever read from the query string.
type fakeAuth struct {
	mu      sync.Mutex
	tools   map[string]*fakeTools
	execs   map[string]ToolExecutor
	expires map[string]time.Time
}

var _ Authenticator = (*fakeAuth)(nil)

func newFakeAuth() *fakeAuth {
	return &fakeAuth{
		tools:   make(map[string]*fakeTools),
		execs:   make(map[string]ToolExecutor),
		expires: map[string]time.Time{"expired": time.Now().Add(-time.Minute)},
	}
}

func (a *fakeAuth) toolsFor(user string) *fakeTools {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tools[user]
	if !ok {
		t = &fakeTools{}
		a.tools[user] = t
	}
	return t
}

// setExecutor replaces the caller-bound tool executor a token resolves to, so a
// proposal test can hand the handler an executor that also plans and applies
// proposals.
func (a *fakeAuth) setExecutor(user string, exec ToolExecutor) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.execs[user] = exec
}

func (a *fakeAuth) Authenticate(r *http.Request) (Caller, error) {
	hdr := r.Header.Get("Authorization")
	const scheme = "Bearer "
	if !strings.HasPrefix(hdr, scheme) {
		return Caller{}, ErrUnauthorized
	}
	user := strings.TrimSpace(strings.TrimPrefix(hdr, scheme))
	switch user {
	case "no-owner":
		// A misconfigured authenticator: nil error, unusable caller.
		return Caller{}, nil
	case "user-a", "user-b", "expired":
	case "forbidden":
		return Caller{}, ErrForbidden
	case "broken":
		return Caller{}, errors.New("auth backend unavailable")
	default:
		return Caller{}, ErrUnauthorized
	}
	a.mu.Lock()
	expiry := a.expires[user]
	exec := a.execs[user]
	tools := a.tools[user]
	if tools == nil {
		tools = &fakeTools{}
		a.tools[user] = tools
	}
	a.mu.Unlock()
	var bound ToolExecutor = tools
	if exec != nil {
		bound = exec
	}
	return Caller{Owner: ownerFor(user), Tools: bound, Expires: expiry}, nil
}

// gatedProvider streams one text chunk and then waits for the test to release
// it, so a test can observe a running turn without any timing race.
type gatedProvider struct {
	release chan struct{}
	started chan struct{}
	ctxDone chan struct{}

	once sync.Once
}

func newGatedProvider() *gatedProvider {
	return &gatedProvider{
		release: make(chan struct{}),
		started: make(chan struct{}),
		ctxDone: make(chan struct{}),
	}
}

func (g *gatedProvider) Name() string { return "gated" }

func (g *gatedProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Chunk, error) {
	g.once.Do(func() { close(g.started) })
	ch := make(chan provider.Chunk, 2)
	ch <- provider.Chunk{Kind: provider.ChunkText, Text: "gated"}
	go func() {
		defer close(ch)
		select {
		case <-g.release:
		case <-ctx.Done():
			close(g.ctxDone)
			return
		}
		select {
		case ch <- provider.Chunk{Kind: provider.ChunkDone}:
		case <-ctx.Done():
			close(g.ctxDone)
		}
	}()
	return ch, nil
}

type fixtureOptions struct {
	steps       []scripted.Step
	provider    provider.Provider
	router      ModelRouter
	models      []string
	maxEvents   int
	maxBody     int64
	maxChars    int
	heartbeat   time.Duration
	targetCheck func(*http.Request, Caller, string) error
	// proposals serves the proposal endpoints and lets the runner store
	// proposals instead of running a planned write.
	proposals       bool
	allowEdits      bool
	decisionTimeout time.Duration
	// uploads serves the upload endpoints and lets turns reference uploads.
	uploads           bool
	maxUploadBytes    int64
	maxAttachments    int
	maxUploadsPerConv int
	// turnContext accepts a page context on turns; setup is the runner hook.
	turnContext bool
	maxContext  int
	setup       func(context.Context, TurnInput) (TurnSetup, error)
	// contextWindow sets RunnerConfig.ContextWindow; compaction turns
	// summaries on.
	contextWindow int
	compaction    bool
}

// apiFixture wires a real handler behind an httptest server, with the same
// store for the runner and the handler.
type apiFixture struct {
	t     *testing.T
	store *MemStore
	prov  *scripted.Provider
	auth  *fakeAuth
	srv   *httptest.Server
}

func newFixture(t *testing.T, opts fixtureOptions) *apiFixture {
	t.Helper()
	store := NewMemStore(MemStoreOptions{MaxEventsPerTurn: opts.maxEvents})

	var (
		prov       provider.Provider
		scriptedPr *scripted.Provider
	)
	if opts.router == nil {
		if opts.provider != nil {
			prov = opts.provider
		} else {
			scriptedPr = scripted.New(opts.steps...)
			prov = scriptedPr
		}
	}
	runnerCfg := RunnerConfig{Store: store, Provider: prov, Model: "model-a", Router: opts.router, Setup: opts.setup}
	runnerCfg.ContextWindow = opts.contextWindow
	if opts.compaction {
		runnerCfg.Compaction = &CompactionConfig{}
	}
	var proposals ProposalStore
	if opts.proposals {
		runnerCfg.Proposals = store
		proposals = store
	}
	var uploads UploadStore
	if opts.uploads {
		runnerCfg.Uploads = store
		uploads = store
	}
	runner := mustRunner(t, runnerCfg)
	auth := newFakeAuth()
	models := opts.models
	if models == nil {
		models = []string{"model-a", "model-b"}
	}
	h, err := NewHandler(HandlerConfig{
		Prefix:                    testPrefix,
		Runner:                    runner,
		Store:                     store,
		Auth:                      auth,
		Router:                    opts.router,
		Models:                    models,
		MaxBodyBytes:              opts.maxBody,
		MaxMessageChars:           opts.maxChars,
		HeartbeatInterval:         opts.heartbeat,
		TargetCheck:               opts.targetCheck,
		Proposals:                 proposals,
		AllowArgumentEdits:        opts.allowEdits,
		DecisionTimeout:           opts.decisionTimeout,
		Uploads:                   uploads,
		MaxUploadBytes:            opts.maxUploadBytes,
		MaxAttachmentsPerTurn:     opts.maxAttachments,
		MaxUploadsPerConversation: opts.maxUploadsPerConv,
		TurnContext:               opts.turnContext,
		MaxContextBytes:           opts.maxContext,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	// Registered after the runner's shutdown cleanup, so the server closes
	// first and no test request outlives the handler.
	t.Cleanup(srv.Close)

	return &apiFixture{
		t:     t,
		store: store,
		prov:  scriptedPr,
		auth:  auth,
		srv:   srv,
	}
}

// with returns a view of the fixture that reports failures on t, so a subtest
// can use the shared server and store without failing its parent.
func (f *apiFixture) with(t *testing.T) *apiFixture {
	clone := *f
	clone.t = t
	return &clone
}

// --- request helpers ---

type apiResponse struct {
	t      *testing.T
	method string
	path   string
	status int
	header http.Header
	body   []byte
}

func (r apiResponse) expect(want int) apiResponse {
	r.t.Helper()
	if r.status != want {
		r.t.Fatalf("%s %s: status %d, want %d, body %s", r.method, r.path, r.status, want, r.body)
	}
	return r
}

func (r apiResponse) decode(out any) {
	r.t.Helper()
	if err := json.Unmarshal(r.body, out); err != nil {
		r.t.Fatalf("%s %s: decode response: %v, body %s", r.method, r.path, err, r.body)
	}
}

func (r apiResponse) errorCode() string {
	r.t.Helper()
	var body errorBody
	r.decode(&body)
	return body.Error.Code
}

// raw issues an authenticated-or-not request and reads the whole response. It
// also asserts the non-cacheable header every response must carry.
func (f *apiFixture) raw(method, target string, body any, hdr map[string]string) apiResponse {
	f.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			f.t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, f.srv.URL+target, reader)
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	for k, v := range hdr {
		if v == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, target, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatalf("%s %s: read body: %v", method, target, err)
	}
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		f.t.Fatalf("%s %s: Cache-Control %q, want non-cacheable (no-store)", method, target, cc)
	}
	return apiResponse{t: f.t, method: method, path: target, status: res.StatusCode, header: res.Header, body: raw}
}

func (f *apiFixture) callAs(token, method, target string, body any, hdr map[string]string) apiResponse {
	f.t.Helper()
	headers := make(map[string]string, len(hdr)+1)
	for k, v := range hdr {
		headers[k] = v
	}
	if _, ok := headers["Authorization"]; !ok {
		headers["Authorization"] = token
	}
	return f.raw(method, target, body, headers)
}

func (f *apiFixture) call(method, target string, body any, hdr map[string]string) apiResponse {
	return f.callAs(tokenA, method, target, body, hdr)
}

func (f *apiFixture) createConversationAs(token string, body any) Conversation {
	f.t.Helper()
	res := f.callAs(token, http.MethodPost, testPrefix+"/conversations", body, nil).expect(http.StatusCreated)
	var conv Conversation
	res.decode(&conv)
	return conv
}

func (f *apiFixture) createConversation(body any) Conversation {
	return f.createConversationAs(tokenA, body)
}

func (f *apiFixture) submitTurnAs(token, convID, text, model string, hdr map[string]string) submitTurnView {
	f.t.Helper()
	body := map[string]any{"text": text}
	if model != "" {
		body["model"] = model
	}
	res := f.callAs(token, http.MethodPost, testPrefix+"/conversations/"+convID+"/turns", body, hdr).
		expect(http.StatusAccepted)
	var out submitTurnView
	res.decode(&out)
	return out
}

func (f *apiFixture) submitTurn(convID, text, model string, hdr map[string]string) submitTurnView {
	return f.submitTurnAs(tokenA, convID, text, model, hdr)
}

func (f *apiFixture) conversation(id string) conversationDetailView {
	f.t.Helper()
	res := f.call(http.MethodGet, testPrefix+"/conversations/"+id, nil, nil).expect(http.StatusOK)
	var out conversationDetailView
	res.decode(&out)
	return out
}

func waitForTurnState(t *testing.T, s Store, owner OwnerKey, turnID string, want TurnState) Turn {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		turn, err := s.GetTurn(context.Background(), owner, turnID)
		if err != nil {
			t.Fatalf("GetTurn(%s): %v", turnID, err)
		}
		if turn.State == want {
			return turn
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s stayed %s, want %s", turnID, turn.State, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// --- SSE reader ---

// sseFrame is one parsed Server-Sent Events frame. Comment frames carry no id
// and no data.
type sseFrame struct {
	id      string
	event   string
	data    string
	comment bool
}

func parseSSEFrame(raw string) sseFrame {
	var (
		frame sseFrame
		data  []string
	)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, ":"):
			frame.comment = true
		case strings.HasPrefix(line, "id:"):
			frame.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			frame.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	frame.data = strings.Join(data, "\n")
	return frame
}

// readSSE reads frames from r with a deliberately tiny buffer, so frames are
// split across reads and multi-byte runes straddle read boundaries.
func readSSE(r io.Reader, emit func(sseFrame) error) error {
	var (
		acc []byte
		buf = make([]byte, 3)
	)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			acc = append(acc, buf[:n]...)
			for {
				idx := bytes.Index(acc, []byte("\n\n"))
				if idx < 0 {
					break
				}
				raw := string(acc[:idx])
				acc = append(acc[:0:0], acc[idx+2:]...)
				if !utf8.ValidString(raw) {
					return fmt.Errorf("sse frame is not valid UTF-8: %q", raw)
				}
				if emitErr := emit(parseSSEFrame(raw)); emitErr != nil {
					return emitErr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// sseStream is one live event-stream response, read incrementally in the
// background so a test can wait on frames or on the end of the stream.
type sseStream struct {
	t    *testing.T
	resp *http.Response
	done chan struct{}

	mu     sync.Mutex
	frames []sseFrame
	closed bool
	err    error
}

func (f *apiFixture) openStream(turnID string, after int64, lastEventID string) *sseStream {
	f.t.Helper()
	target := testPrefix + "/turns/" + turnID + "/events"
	if after > 0 {
		target += "?after=" + strconv.FormatInt(after, 10)
	}
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+target, nil)
	if err != nil {
		f.t.Fatalf("new stream request: %v", err)
	}
	req.Header.Set("Authorization", tokenA)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	res, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatalf("open stream: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		f.t.Fatalf("open stream: status %d, body %s", res.StatusCode, raw)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		res.Body.Close()
		f.t.Fatalf("stream Content-Type %q, want text/event-stream", ct)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		res.Body.Close()
		f.t.Fatalf("stream Cache-Control %q, want no-store", cc)
	}
	if ab := res.Header.Get("X-Accel-Buffering"); ab != "no" {
		res.Body.Close()
		f.t.Fatalf("stream X-Accel-Buffering %q, want no", ab)
	}

	s := &sseStream{t: f.t, resp: res, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		s.err = readSSE(res.Body, func(frame sseFrame) error {
			s.mu.Lock()
			s.frames = append(s.frames, frame)
			s.mu.Unlock()
			return nil
		})
	}()
	f.t.Cleanup(s.close)
	return s
}

func (s *sseStream) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.resp.Body.Close()
}

func (s *sseStream) snapshot() []sseFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sseFrame, len(s.frames))
	copy(out, s.frames)
	return out
}

func (s *sseStream) waitFrames(n int, wait time.Duration) []sseFrame {
	s.t.Helper()
	deadline := time.Now().Add(wait)
	for {
		frames := s.snapshot()
		if len(frames) >= n {
			return frames
		}
		select {
		case <-s.done:
			s.t.Fatalf("stream ended after %d frames, want %d", len(frames), n)
		default:
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("stream delivered %d frames within %s, want %d", len(frames), wait, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *sseStream) waitDone(wait time.Duration) []sseFrame {
	s.t.Helper()
	select {
	case <-s.done:
	case <-time.After(wait):
		s.t.Fatalf("stream did not end within %s after %d frames", wait, len(s.snapshot()))
	}
	s.mu.Lock()
	closed, err := s.closed, s.err
	s.mu.Unlock()
	if !closed && err != nil {
		s.t.Fatalf("stream ended with error: %v", err)
	}
	return s.snapshot()
}

// events decodes the event frames and checks the id and event fields against
// the envelope, so the sequence carried in SSE can never drift from the JSON.
func (s *sseStream) events() []Event {
	s.t.Helper()
	var out []Event
	for _, frame := range s.snapshot() {
		if frame.comment {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(frame.data), &ev); err != nil {
			s.t.Fatalf("decode event frame: %v, data %s", err, frame.data)
		}
		if frame.event != string(ev.Type) {
			s.t.Fatalf("frame event %q, envelope type %q", frame.event, ev.Type)
		}
		if frame.id != strconv.FormatInt(ev.Seq, 10) {
			s.t.Fatalf("frame id %q, envelope seq %d", frame.id, ev.Seq)
		}
		out = append(out, ev)
	}
	return out
}

func (s *sseStream) pings() int {
	n := 0
	for _, frame := range s.snapshot() {
		if frame.comment {
			n++
		}
	}
	return n
}

// applyStream folds streamed events into a view, failing on the first gap or
// out-of-order event.
func applyStream(t *testing.T, events []Event) TurnView {
	t.Helper()
	var view TurnView
	for _, ev := range events {
		if err := view.Apply(ev); err != nil {
			t.Fatalf("apply event %d (%s): %v", ev.Seq, ev.Type, err)
		}
	}
	return view
}

// --- tests ---

func TestNewHandlerValidatesConfig(t *testing.T) {
	store := NewMemStore(MemStoreOptions{})
	runner := mustRunner(t, RunnerConfig{Store: store, Provider: scripted.New()})
	auth := newFakeAuth()
	base := HandlerConfig{Prefix: testPrefix, Runner: runner, Store: store, Auth: auth}

	if _, err := NewHandler(base); err != nil {
		t.Fatalf("NewHandler with a complete config: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*HandlerConfig)
	}{
		{"no runner", func(c *HandlerConfig) { c.Runner = nil }},
		{"no store", func(c *HandlerConfig) { c.Store = nil }},
		{"no auth", func(c *HandlerConfig) { c.Auth = nil }},
		{"no prefix", func(c *HandlerConfig) { c.Prefix = "" }},
		{"relative prefix", func(c *HandlerConfig) { c.Prefix = "api/chat" }},
		{"trailing slash", func(c *HandlerConfig) { c.Prefix = testPrefix + "/" }},
		{"root prefix", func(c *HandlerConfig) { c.Prefix = "/" }},
		{"unclean prefix", func(c *HandlerConfig) { c.Prefix = "/api//chat" }},
		{"wildcard prefix", func(c *HandlerConfig) { c.Prefix = "/api/{tenant}/chat" }},
		{"query in prefix", func(c *HandlerConfig) { c.Prefix = "/api/chat?x=1" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			if _, err := NewHandler(cfg); err == nil {
				t.Fatalf("NewHandler accepted an invalid config")
			}
		})
	}
}

func TestHTTPStatus(t *testing.T) {
	f := newFixture(t, fixtureOptions{models: []string{"model-b", "model-a", "model-b", " "}})
	res := f.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK)
	if ct := res.header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type %q", ct)
	}
	var st statusView
	res.decode(&st)
	if st.Version != ProtocolVersion {
		t.Fatalf("version %q, want %q", st.Version, ProtocolVersion)
	}
	if strings.Join(st.Models, ",") != "model-b,model-a" {
		t.Fatalf("models %v, want the normalized allowlist", st.Models)
	}
	if st.Features.Proposals || st.Features.Uploads {
		t.Fatalf("features %+v, want both disabled in v1.1", st.Features)
	}
	if st.Limits.MaxMessageChars != defaultMaxMessageChars {
		t.Fatalf("maxMessageChars %d, want %d", st.Limits.MaxMessageChars, defaultMaxMessageChars)
	}

	// An empty allowlist must serialize as [] rather than null.
	empty := newFixture(t, fixtureOptions{models: []string{}})
	raw := empty.call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK)
	if !bytes.Contains(raw.body, []byte(`"models":[]`)) {
		t.Fatalf("empty models serialized as %s", raw.body)
	}
}

func TestHTTPAuthentication(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	t.Run("missing credentials", func(t *testing.T) {
		res := f.with(t).callAs("", http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusUnauthorized)
		if got := res.header.Get("WWW-Authenticate"); got != "Bearer" {
			t.Fatalf("WWW-Authenticate %q, want Bearer", got)
		}
		if got := res.errorCode(); got != CodeUnauthorized {
			t.Fatalf("code %q, want %q", got, CodeUnauthorized)
		}
	})

	t.Run("unknown credentials", func(t *testing.T) {
		res := f.with(t).callAs("Bearer nobody", http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusUnauthorized)
		if got := res.header.Get("WWW-Authenticate"); got != "Bearer" {
			t.Fatalf("WWW-Authenticate %q, want Bearer", got)
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		res := f.with(t).callAs(tokenForbidden, http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusForbidden)
		if got := res.errorCode(); got != CodeForbidden {
			t.Fatalf("code %q, want %q", got, CodeForbidden)
		}
		if got := res.header.Get("WWW-Authenticate"); got != "" {
			t.Fatalf("WWW-Authenticate %q, want none", got)
		}
	})

	t.Run("authenticator failure", func(t *testing.T) {
		res := f.with(t).callAs(tokenBroken, http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusInternalServerError)
		if got := res.errorCode(); got != CodeInternal {
			t.Fatalf("code %q, want %q", got, CodeInternal)
		}
		if bytes.Contains(res.body, []byte("auth backend")) {
			t.Fatalf("internal error text leaked: %s", res.body)
		}
	})

	t.Run("authenticator without an owner", func(t *testing.T) {
		res := f.with(t).callAs("Bearer no-owner", http.MethodGet, testPrefix+"/status", nil, nil).
			expect(http.StatusInternalServerError)
		if got := res.errorCode(); got != CodeInternal {
			t.Fatalf("code %q, want %q", got, CodeInternal)
		}
	})

	t.Run("token in query", func(t *testing.T) {
		for _, param := range []string{"token", "access_token", "auth"} {
			target := testPrefix + "/status?" + param + "=secret"
			res := f.with(t).call(http.MethodGet, target, nil, nil).expect(http.StatusBadRequest)
			if got := res.errorCode(); got != CodeInvalidRequest {
				t.Fatalf("%s: code %q, want %q", param, got, CodeInvalidRequest)
			}
			if bytes.Contains(res.body, []byte("secret")) {
				t.Fatalf("%s: response echoed the token: %s", param, res.body)
			}
		}
	})

	t.Run("authenticated", func(t *testing.T) {
		f.with(t).call(http.MethodGet, testPrefix+"/status", nil, nil).expect(http.StatusOK)
	})
}

func TestHTTPRoutingErrors(t *testing.T) {
	f := newFixture(t, fixtureOptions{})

	t.Run("method not allowed", func(t *testing.T) {
		res := f.with(t).call(http.MethodPut, testPrefix+"/status", nil, nil).expect(http.StatusMethodNotAllowed)
		if got := res.errorCode(); got != CodeMethodNotAllowed {
			t.Fatalf("code %q, want %q", got, CodeMethodNotAllowed)
		}
		if allow := res.header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
			t.Fatalf("Allow %q, want GET", allow)
		}
	})

	t.Run("unknown path", func(t *testing.T) {
		scoped := f.with(t)
		res := scoped.call(http.MethodGet, testPrefix+"/nothing-here", nil, nil).expect(http.StatusNotFound)
		if got := res.errorCode(); got != CodeNotFound {
			t.Fatalf("code %q, want %q", got, CodeNotFound)
		}
		scoped.call(http.MethodGet, testPrefix, nil, nil).expect(http.StatusNotFound)
	})

	t.Run("anonymous unknown path", func(t *testing.T) {
		f.with(t).callAs("", http.MethodGet, testPrefix+"/nothing-here", nil, nil).expect(http.StatusUnauthorized)
	})
}

func TestHTTPConversationListAndRename(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	created := []Conversation{
		f.createConversation(map[string]any{"title": "one"}),
		f.createConversation(map[string]any{"title": "two"}),
		f.createConversation(nil),
	}

	first := f.call(http.MethodGet, testPrefix+"/conversations?limit=2", nil, nil).expect(http.StatusOK)
	var page conversationListView
	first.decode(&page)
	if len(page.Items) != 2 {
		t.Fatalf("first page has %d items, want 2", len(page.Items))
	}
	if page.NextCursor == "" {
		t.Fatal("first page reported no cursor")
	}
	second := f.call(http.MethodGet, testPrefix+"/conversations?limit=2&cursor="+page.NextCursor, nil, nil).
		expect(http.StatusOK)
	var rest conversationListView
	second.decode(&rest)
	if len(rest.Items) != 1 || rest.NextCursor != "" {
		t.Fatalf("second page has %d items, cursor %q", len(rest.Items), rest.NextCursor)
	}

	seen := map[string]bool{}
	for _, item := range append(page.Items, rest.Items...) {
		if seen[item.ID] {
			t.Fatalf("conversation %s appears on both pages", item.ID)
		}
		seen[item.ID] = true
	}
	for _, conv := range created {
		if !seen[conv.ID] {
			t.Fatalf("conversation %s missing from the paged list", conv.ID)
		}
	}

	res := f.call(http.MethodPatch, testPrefix+"/conversations/"+created[0].ID,
		map[string]any{"title": "  renamed  "}, nil).expect(http.StatusOK)
	var renamed Conversation
	res.decode(&renamed)
	if renamed.Title != "renamed" || renamed.ID != created[0].ID {
		t.Fatalf("renamed conversation %+v", renamed)
	}
	f.call(http.MethodPatch, testPrefix+"/conversations/"+created[0].ID,
		map[string]any{"title": ""}, nil).expect(http.StatusOK)

	f.call(http.MethodPatch, testPrefix+"/conversations/000000000000000",
		map[string]any{"title": "x"}, nil).expect(http.StatusNotFound)
}

func TestHTTPFullFlow(t *testing.T) {
	steps := []scripted.Step{{Chunks: []provider.Chunk{
		{Kind: provider.ChunkText, Text: "héllo "},
		{Kind: provider.ChunkText, Text: "🌍 "},
		{Kind: provider.ChunkText, Text: "世界"},
		{Kind: provider.ChunkUsage, Usage: &provider.Usage{InputTokens: 3, OutputTokens: 4}},
		{Kind: provider.ChunkDone},
	}}}
	f := newFixture(t, fixtureOptions{steps: steps})
	owner := ownerFor("user-a")

	conv := f.createConversation(map[string]any{"title": "  Greeting  "})
	if conv.ID == "" {
		t.Fatalf("created conversation %+v", conv)
	}
	if conv.Title != "Greeting" {
		t.Fatalf("title %q, want trimmed %q", conv.Title, "Greeting")
	}

	sub := f.submitTurn(conv.ID, "  say hi  ", "", nil)
	if !sub.Created {
		t.Fatalf("first submission reported created=false")
	}
	if sub.Turn.Model != "model-a" {
		t.Fatalf("turn model %q, want the runner default", sub.Turn.Model)
	}
	if sub.Turn.State != TurnStateQueued {
		t.Fatalf("turn state %q, want queued", sub.Turn.State)
	}

	stream := f.openStream(sub.Turn.ID, 0, "")
	stream.waitDone(testWait)
	events := stream.events()
	if len(events) == 0 {
		t.Fatal("stream delivered no events")
	}
	view := applyStream(t, events)
	if view.State != TurnStateCompleted {
		t.Fatalf("turn ended %q, want completed", view.State)
	}
	if view.Text != "héllo 🌍 世界" {
		t.Fatalf("reduced text %q", view.Text)
	}
	if view.Usage == nil || view.Usage.OutputTokens != 4 {
		t.Fatalf("usage %+v", view.Usage)
	}

	stored := mustEvents(t, f.store, owner, sub.Turn.ID)
	if len(stored) != len(events) {
		t.Fatalf("stream carried %d events, store holds %d", len(events), len(stored))
	}
	for i := range stored {
		if stored[i].Seq != events[i].Seq || stored[i].Type != events[i].Type {
			t.Fatalf("event %d: stream %d/%s, store %d/%s", i, events[i].Seq, events[i].Type, stored[i].Seq, stored[i].Type)
		}
	}

	detail := f.conversation(conv.ID)
	if detail.Conversation.ID != conv.ID || detail.Conversation.Title != "Greeting" {
		t.Fatalf("detail conversation %+v", detail.Conversation)
	}
	if detail.ActiveTurn != nil {
		t.Fatalf("activeTurn %+v after completion", detail.ActiveTurn)
	}
	if len(detail.Messages) != 2 {
		t.Fatalf("transcript has %d messages, want 2", len(detail.Messages))
	}
	if detail.Messages[0].Role != RoleUser || detail.Messages[0].Parts[0].Text != "say hi" {
		t.Fatalf("user message %+v", detail.Messages[0])
	}
	if detail.Messages[1].Role != RoleAssistant || detail.Messages[1].Parts[0].Text != "héllo 🌍 世界" {
		t.Fatalf("assistant message %+v", detail.Messages[1])
	}

	// The transcript pages with a cursor like the conversation list.
	firstPage := f.call(http.MethodGet, testPrefix+"/conversations/"+conv.ID+"?limit=1", nil, nil).
		expect(http.StatusOK)
	var pageOne conversationDetailView
	firstPage.decode(&pageOne)
	if len(pageOne.Messages) != 1 || pageOne.NextCursor == "" {
		t.Fatalf("first transcript page has %d messages, cursor %q", len(pageOne.Messages), pageOne.NextCursor)
	}
	secondPage := f.call(http.MethodGet, testPrefix+"/conversations/"+conv.ID+"?limit=1&cursor="+pageOne.NextCursor, nil, nil).
		expect(http.StatusOK)
	var pageTwo conversationDetailView
	secondPage.decode(&pageTwo)
	if len(pageTwo.Messages) != 1 || pageTwo.NextCursor != "" {
		t.Fatalf("second transcript page has %d messages, cursor %q", len(pageTwo.Messages), pageTwo.NextCursor)
	}
	if pageTwo.Messages[0].ID == pageOne.Messages[0].ID {
		t.Fatal("transcript pages repeated the same message")
	}

	// The stored turn is the authoritative state.
	res := f.call(http.MethodGet, testPrefix+"/turns/"+sub.Turn.ID, nil, nil).expect(http.StatusOK)
	var turn Turn
	res.decode(&turn)
	if turn.State != TurnStateCompleted {
		t.Fatalf("GET turn state %q", turn.State)
	}
}

func TestHTTPModelSelection(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		steps:  []scripted.Step{textStep("one"), textStep("two")},
		models: []string{"model-a", "model-b"},
	})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)

	first := f.submitTurn(conv.ID, "first", "model-b", nil)
	if first.Turn.Model != "model-b" {
		t.Fatalf("turn model %q, want model-b", first.Turn.Model)
	}
	awaitTerminal(t, f.store, owner, first.Turn.ID)

	second := f.submitTurn(conv.ID, "second", "", nil)
	if second.Turn.Model != "model-a" {
		t.Fatalf("turn model %q, want the runner default", second.Turn.Model)
	}
	awaitTerminal(t, f.store, owner, second.Turn.ID)

	reqs := f.prov.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider served %d requests, want 2", len(reqs))
	}
	if reqs[0].Model != "model-b" || reqs[1].Model != "model-a" {
		t.Fatalf("provider models %q, %q", reqs[0].Model, reqs[1].Model)
	}
}

func TestHTTPCredentialDeadline(t *testing.T) {
	f := newFixture(t, fixtureOptions{steps: []scripted.Step{textStep("never sent")}})
	owner := ownerFor("expired")

	conv := f.createConversationAs(tokenExpired, nil)
	sub := f.submitTurnAs(tokenExpired, conv.ID, "hi", "", nil)

	turn := awaitTerminal(t, f.store, owner, sub.Turn.ID)
	if turn.State != TurnStateFailed {
		t.Fatalf("turn state %q, want failed", turn.State)
	}
	if turn.Error == nil || turn.Error.Code != FailureAuthExpired {
		t.Fatalf("turn error %+v, want %s", turn.Error, FailureAuthExpired)
	}
	if reqs := f.prov.Requests(); len(reqs) != 0 {
		t.Fatalf("provider was called %d times with an expired credential", len(reqs))
	}
}

func TestHTTPResume(t *testing.T) {
	f := newFixture(t, fixtureOptions{steps: []scripted.Step{{
		Delay: 100 * time.Millisecond,
		Chunks: []provider.Chunk{
			{Kind: provider.ChunkText, Text: "a"},
			{Kind: provider.ChunkText, Text: "b"},
			{Kind: provider.ChunkText, Text: "c"},
			{Kind: provider.ChunkDone},
		},
	}}})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	sub := f.submitTurn(conv.ID, "hi", "", nil)

	first := f.openStream(sub.Turn.ID, 0, "")
	frames := first.waitFrames(2, testWait)
	lastID := frames[len(frames)-1].id
	if lastID == "" {
		t.Fatal("first frames carried no event id")
	}
	first.close()

	second := f.openStream(sub.Turn.ID, 0, lastID)
	second.waitDone(testWait)

	seen := map[int64]int{}
	var combined []Event
	combined = append(combined, first.events()...)
	rest := second.events()
	if len(rest) == 0 {
		t.Fatal("resumed stream delivered no events")
	}
	wantFirst, err := strconv.ParseInt(lastID, 10, 64)
	if err != nil {
		t.Fatalf("parse last event id %q: %v", lastID, err)
	}
	if rest[0].Seq != wantFirst+1 {
		t.Fatalf("resumed stream started at %d, want %d", rest[0].Seq, wantFirst+1)
	}
	combined = append(combined, rest...)
	for _, ev := range combined {
		seen[ev.Seq]++
	}

	var view TurnView
	for _, ev := range combined {
		if err := view.Apply(ev); err != nil {
			t.Fatalf("apply event %d: %v", ev.Seq, err)
		}
	}
	if view.State != TurnStateCompleted || view.Text != "abc" {
		t.Fatalf("resumed view %q text %q", view.State, view.Text)
	}

	stored := mustEvents(t, f.store, owner, sub.Turn.ID)
	if len(seen) != len(stored) {
		t.Fatalf("received %d distinct events, store holds %d", len(seen), len(stored))
	}
	for seq, n := range seen {
		if n != 1 {
			t.Fatalf("event %d received %d times", seq, n)
		}
		if seq < 1 || seq > int64(len(stored)) {
			t.Fatalf("event %d outside the stored range", seq)
		}
	}
	if view.LastSeq != int64(len(stored)) {
		t.Fatalf("last applied sequence %d, want %d", view.LastSeq, len(stored))
	}

	// A cursor past the end of a finished turn ends the response cleanly
	// instead of hanging or failing.
	beyond := f.openStream(sub.Turn.ID, view.LastSeq+100, "")
	beyond.waitDone(testWait)
	if events := beyond.events(); len(events) != 0 {
		t.Fatalf("cursor past the end delivered %d events", len(events))
	}
}

func TestHTTPResyncRequired(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		steps:     []scripted.Step{textStep("x")},
		maxEvents: 1,
	})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	sub := f.submitTurn(conv.ID, "hi", "", nil)
	awaitTerminal(t, f.store, owner, sub.Turn.ID)

	res := f.call(http.MethodGet, testPrefix+"/turns/"+sub.Turn.ID+"/events?after=0", nil, nil).
		expect(http.StatusConflict)
	if ct := res.header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type %q, want JSON: the stream must not start", ct)
	}
	if got := res.errorCode(); got != CodeResyncRequired {
		t.Fatalf("code %q, want %q", got, CodeResyncRequired)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(res.body), []byte("{")) {
		t.Fatalf("response is not a JSON error: %s", res.body)
	}
}

func TestHTTPIdempotentSubmit(t *testing.T) {
	f := newFixture(t, fixtureOptions{steps: []scripted.Step{{
		Chunks:           []provider.Chunk{{Kind: provider.ChunkText, Text: "working"}},
		BlockUntilCancel: true,
	}}})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	header := map[string]string{"Idempotency-Key": "turn-key-1"}

	first := f.submitTurn(conv.ID, "hi", "", header)
	if !first.Created {
		t.Fatal("first submission reported created=false")
	}
	waitForTurnState(t, f.store, owner, first.Turn.ID, TurnStateRunning)

	again := f.submitTurn(conv.ID, "hi", "", header)
	if again.Created {
		t.Fatal("replayed submission reported created=true")
	}
	if again.Turn.ID != first.Turn.ID {
		t.Fatalf("replayed turn %q, want %q", again.Turn.ID, first.Turn.ID)
	}
	if reqs := f.prov.Requests(); len(reqs) != 1 {
		t.Fatalf("provider served %d requests for one idempotent turn, want 1", len(reqs))
	}
	detail := f.conversation(conv.ID)
	if detail.ActiveTurn == nil || detail.ActiveTurn.ID != first.Turn.ID {
		t.Fatalf("activeTurn %+v, want the running turn", detail.ActiveTurn)
	}

	// A different key is a new turn, and the conversation already has one.
	res := f.call(http.MethodPost, testPrefix+"/conversations/"+conv.ID+"/turns",
		map[string]any{"text": "second"}, map[string]string{"Idempotency-Key": "turn-key-2"}).
		expect(http.StatusConflict)
	if got := res.errorCode(); got != CodeActiveTurn {
		t.Fatalf("code %q, want %q", got, CodeActiveTurn)
	}
}

func TestHTTPCancel(t *testing.T) {
	f := newFixture(t, fixtureOptions{steps: []scripted.Step{{
		Chunks:           []provider.Chunk{{Kind: provider.ChunkText, Text: "thinking"}},
		BlockUntilCancel: true,
	}}})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	sub := f.submitTurn(conv.ID, "please stop", "", nil)

	stream := f.openStream(sub.Turn.ID, 0, "")
	waitForTurnState(t, f.store, owner, sub.Turn.ID, TurnStateRunning)

	res := f.call(http.MethodPost, testPrefix+"/turns/"+sub.Turn.ID+"/cancel", nil, nil).expect(http.StatusOK)
	var cancelled Turn
	res.decode(&cancelled)
	if cancelled.State != TurnStateCancelled {
		t.Fatalf("cancel returned state %q, want cancelled", cancelled.State)
	}

	stream.waitDone(testWait)
	events := stream.events()
	if len(events) == 0 {
		t.Fatal("stream delivered no events")
	}
	last := events[len(events)-1]
	if last.Type != EventTurnCancelled {
		t.Fatalf("stream ended with %s, want %s", last.Type, EventTurnCancelled)
	}
	view := applyStream(t, events)
	if view.State != TurnStateCancelled {
		t.Fatalf("view state %q, want cancelled", view.State)
	}

	// Cancelling a terminal turn is a no-op that still succeeds.
	res = f.call(http.MethodPost, testPrefix+"/turns/"+sub.Turn.ID+"/cancel", nil, nil).expect(http.StatusOK)
	var unchanged Turn
	res.decode(&unchanged)
	if unchanged.ID != cancelled.ID || unchanged.State != TurnStateCancelled {
		t.Fatalf("second cancel returned %+v, want the stored turn", unchanged)
	}
}

func TestHTTPDisconnectDoesNotCancelTurn(t *testing.T) {
	gated := newGatedProvider()
	f := newFixture(t, fixtureOptions{provider: gated})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	sub := f.submitTurn(conv.ID, "hi", "", nil)

	stream := f.openStream(sub.Turn.ID, 0, "")
	stream.waitFrames(1, testWait)
	stream.close()

	select {
	case <-gated.started:
	case <-time.After(testWait):
		t.Fatal("the turn never reached the provider")
	}
	if turn, err := f.store.GetTurn(context.Background(), owner, sub.Turn.ID); err != nil {
		t.Fatalf("GetTurn: %v", err)
	} else if turn.State.Terminal() {
		t.Fatalf("turn is %q before the provider was released", turn.State)
	}

	// The turn must still be able to finish after the reader went away.
	close(gated.release)
	turn := awaitTerminal(t, f.store, owner, sub.Turn.ID)
	if turn.State != TurnStateCompleted {
		t.Fatalf("turn state %q, want completed", turn.State)
	}
}

func TestHTTPOwnershipIsolation(t *testing.T) {
	f := newFixture(t, fixtureOptions{steps: []scripted.Step{textStep("hi back")}})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	sub := f.submitTurn(conv.ID, "hi", "", nil)
	awaitTerminal(t, f.store, owner, sub.Turn.ID)

	const (
		missingConv = "000000000000000"
		missingTurn = "000000000000001"
	)
	type pair struct {
		method string
		body   any
		other  string
		absent string
	}
	pairs := []pair{
		{http.MethodGet, nil, testPrefix + "/conversations/" + conv.ID, testPrefix + "/conversations/" + missingConv},
		{http.MethodPatch, map[string]any{"title": "x"}, testPrefix + "/conversations/" + conv.ID, testPrefix + "/conversations/" + missingConv},
		{http.MethodDelete, nil, testPrefix + "/conversations/" + conv.ID, testPrefix + "/conversations/" + missingConv},
		{http.MethodPost, map[string]any{"text": "hi"}, testPrefix + "/conversations/" + conv.ID + "/turns", testPrefix + "/conversations/" + missingConv + "/turns"},
		{http.MethodGet, nil, testPrefix + "/turns/" + sub.Turn.ID, testPrefix + "/turns/" + missingTurn},
		{http.MethodGet, nil, testPrefix + "/turns/" + sub.Turn.ID + "/events", testPrefix + "/turns/" + missingTurn + "/events"},
		{http.MethodPost, nil, testPrefix + "/turns/" + sub.Turn.ID + "/cancel", testPrefix + "/turns/" + missingTurn + "/cancel"},
	}
	for _, p := range pairs {
		t.Run(p.method+" "+p.other, func(t *testing.T) {
			scoped := f.with(t)
			other := scoped.callAs(tokenB, p.method, p.other, p.body, nil).expect(http.StatusNotFound)
			absent := scoped.call(p.method, p.absent, p.body, nil).expect(http.StatusNotFound)
			if !bytes.Equal(other.body, absent.body) {
				t.Fatalf("other-owner body %s differs from missing-record body %s", other.body, absent.body)
			}
			if got := other.errorCode(); got != CodeNotFound {
				t.Fatalf("code %q, want %q", got, CodeNotFound)
			}
			if ct := other.header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type %q", ct)
			}
		})
	}

	// The other owner's list is empty, and the first owner's records survive.
	list := f.callAs(tokenB, http.MethodGet, testPrefix+"/conversations", nil, nil).expect(http.StatusOK)
	var listed conversationListView
	list.decode(&listed)
	if len(listed.Items) != 0 {
		t.Fatalf("user-b sees %d conversations, want 0", len(listed.Items))
	}
	f.conversation(conv.ID)
}

func TestHTTPValidation(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		steps:    []scripted.Step{textStep("ok")},
		maxChars: 5,
	})
	conv := f.createConversation(nil)
	turns := testPrefix + "/conversations/" + conv.ID + "/turns"
	longTitle := strings.Repeat("t", maxTitleRunes+1)

	cases := []struct {
		name   string
		method string
		target string
		body   any
		status int
		code   string
	}{
		{"empty text", http.MethodPost, turns, map[string]any{"text": ""}, http.StatusBadRequest, CodeInvalidRequest},
		{"blank text", http.MethodPost, turns, map[string]any{"text": "   "}, http.StatusBadRequest, CodeInvalidRequest},
		{"missing text", http.MethodPost, turns, map[string]any{}, http.StatusBadRequest, CodeInvalidRequest},
		{"no body", http.MethodPost, turns, "", http.StatusBadRequest, CodeInvalidRequest},
		{"text too long", http.MethodPost, turns, map[string]any{"text": "123456"}, http.StatusBadRequest, CodeInvalidRequest},
		{"unknown field", http.MethodPost, turns, map[string]any{"text": "hi", "extra": true}, http.StatusBadRequest, CodeInvalidRequest},
		{"trailing data", http.MethodPost, turns, `{"text":"hi"}{"text":"ho"}`, http.StatusBadRequest, CodeInvalidRequest},
		{"unknown model", http.MethodPost, turns, map[string]any{"text": "hi", "model": "model-z"}, http.StatusBadRequest, CodeInvalidRequest},
		{"oversized body", http.MethodPost, turns, `{"text":"` + strings.Repeat("a", 70<<10) + `"}`, http.StatusRequestEntityTooLarge, CodePayloadTooLarge},
		{"bad limit", http.MethodGet, testPrefix + "/conversations?limit=x", nil, http.StatusBadRequest, CodeInvalidRequest},
		{"negative limit", http.MethodGet, testPrefix + "/conversations?limit=-1", nil, http.StatusBadRequest, CodeInvalidRequest},
		{"bad cursor", http.MethodGet, testPrefix + "/conversations?cursor=%21%21", nil, http.StatusBadRequest, CodeInvalidRequest},
		{"bad message cursor", http.MethodGet, testPrefix + "/conversations/" + conv.ID + "?cursor=%21%21", nil, http.StatusBadRequest, CodeInvalidRequest},
		{"title missing", http.MethodPatch, testPrefix + "/conversations/" + conv.ID, map[string]any{}, http.StatusBadRequest, CodeInvalidRequest},
		{"title wrong type", http.MethodPatch, testPrefix + "/conversations/" + conv.ID, map[string]any{"title": 7}, http.StatusBadRequest, CodeInvalidRequest},
		{"title too long", http.MethodPatch, testPrefix + "/conversations/" + conv.ID, map[string]any{"title": longTitle}, http.StatusBadRequest, CodeInvalidRequest},
		{"create title too long", http.MethodPost, testPrefix + "/conversations", map[string]any{"title": longTitle}, http.StatusBadRequest, CodeInvalidRequest},
		{"create unknown field", http.MethodPost, testPrefix + "/conversations", map[string]any{"name": "x"}, http.StatusBadRequest, CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := f.with(t).call(tc.method, tc.target, tc.body, nil).expect(tc.status)
			if got := res.errorCode(); got != tc.code {
				t.Fatalf("code %q, want %q, body %s", got, tc.code, res.body)
			}
		})
	}

	// A limit within range is accepted, and an accepted message still works
	// after all the rejected requests.
	f.call(http.MethodGet, testPrefix+"/conversations?limit=1", nil, nil).expect(http.StatusOK)
	sub := f.submitTurn(conv.ID, "hi", "", nil)
	awaitTerminal(t, f.store, ownerFor("user-a"), sub.Turn.ID)

	for _, after := range []string{"abc", "-1"} {
		res := f.call(http.MethodGet, testPrefix+"/turns/"+sub.Turn.ID+"/events?after="+after, nil, nil).
			expect(http.StatusBadRequest)
		if got := res.errorCode(); got != CodeInvalidRequest {
			t.Fatalf("after=%s: code %q", after, got)
		}
	}
}

func TestHTTPDeleteActiveTurn(t *testing.T) {
	gated := newGatedProvider()
	f := newFixture(t, fixtureOptions{provider: gated})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	sub := f.submitTurn(conv.ID, "hi", "", nil)
	waitForTurnState(t, f.store, owner, sub.Turn.ID, TurnStateRunning)

	res := f.call(http.MethodDelete, testPrefix+"/conversations/"+conv.ID, nil, nil).expect(http.StatusNoContent)
	if len(res.body) != 0 {
		t.Fatalf("204 carried a body: %s", res.body)
	}

	// The active turn was cancelled, which is what let the delete succeed.
	select {
	case <-gated.ctxDone:
	case <-time.After(testWait):
		t.Fatal("the active turn was not cancelled before deletion")
	}

	f.call(http.MethodGet, testPrefix+"/conversations/"+conv.ID, nil, nil).expect(http.StatusNotFound)
	f.call(http.MethodGet, testPrefix+"/turns/"+sub.Turn.ID, nil, nil).expect(http.StatusNotFound)
	f.call(http.MethodDelete, testPrefix+"/conversations/"+conv.ID, nil, nil).expect(http.StatusNotFound)
}

func TestHTTPHeartbeat(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		steps: []scripted.Step{{
			Delay: 300 * time.Millisecond,
			Chunks: []provider.Chunk{
				{Kind: provider.ChunkText, Text: "slow"},
				{Kind: provider.ChunkDone},
			},
		}},
		heartbeat: 20 * time.Millisecond,
	})
	owner := ownerFor("user-a")
	conv := f.createConversation(nil)
	sub := f.submitTurn(conv.ID, "hi", "", nil)

	stream := f.openStream(sub.Turn.ID, 0, "")
	stream.waitDone(testWait)
	if pings := stream.pings(); pings == 0 {
		t.Fatal("no heartbeat comment was sent during the slow step")
	}
	for _, frame := range stream.snapshot() {
		if frame.comment && frame.id != "" {
			t.Fatalf("heartbeat carried an id %q and would advance the sequence", frame.id)
		}
	}

	view := applyStream(t, stream.events())
	if view.State != TurnStateCompleted {
		t.Fatalf("view state %q, want completed", view.State)
	}
	stored := mustEvents(t, f.store, owner, sub.Turn.ID)
	if view.LastSeq != int64(len(stored)) {
		t.Fatalf("heartbeats advanced the sequence: last applied %d, stored %d", view.LastSeq, len(stored))
	}
}

// targetPolicy is a revocable TargetCheck stand-in.
type targetPolicy struct {
	mu      sync.Mutex
	allowed map[string]bool
	users   []string
}

func newTargetPolicy(allowed ...string) *targetPolicy {
	p := &targetPolicy{allowed: make(map[string]bool, len(allowed))}
	for _, target := range allowed {
		p.allowed[target] = true
	}
	return p
}

func (p *targetPolicy) check(r *http.Request, c Caller, target string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users = append(p.users, c.Owner.UserID)
	if p.allowed[target] {
		return nil
	}
	return ErrForbidden
}

func (p *targetPolicy) revoke(target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.allowed, target)
}

func (p *targetPolicy) checkedUsers() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.users))
	copy(out, p.users)
	return out
}

func TestHTTPTargetsUnsupported(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	res := f.call(http.MethodPost, testPrefix+"/conversations",
		map[string]any{"target": "site-1"}, nil).expect(http.StatusBadRequest)
	if got := res.errorCode(); got != CodeInvalidRequest {
		t.Fatalf("code %q, want %q", got, CodeInvalidRequest)
	}
	f.call(http.MethodGet, testPrefix+"/conversations?target=site-1", nil, nil).expect(http.StatusBadRequest)
	f.createConversation(nil)
}

func TestHTTPTargetRecheck(t *testing.T) {
	policy := newTargetPolicy("site-1")
	f := newFixture(t, fixtureOptions{
		steps:       []scripted.Step{textStep("hi back")},
		targetCheck: policy.check,
	})
	owner := ownerFor("user-a")

	denied := f.call(http.MethodPost, testPrefix+"/conversations",
		map[string]any{"target": "site-2"}, nil).expect(http.StatusForbidden)
	if got := denied.errorCode(); got != CodeForbidden {
		t.Fatalf("code %q, want %q", got, CodeForbidden)
	}

	conv := f.createConversation(map[string]any{"title": "Site chat", "target": "site-1"})
	if conv.Target != "site-1" {
		t.Fatalf("target %q, want site-1", conv.Target)
	}
	f.conversation(conv.ID)

	list := f.call(http.MethodGet, testPrefix+"/conversations?target=site-1", nil, nil).expect(http.StatusOK)
	var listed conversationListView
	list.decode(&listed)
	if len(listed.Items) != 1 || listed.Items[0].ID != conv.ID {
		t.Fatalf("filtered list %+v", listed.Items)
	}
	f.call(http.MethodGet, testPrefix+"/conversations?target=site-2", nil, nil).expect(http.StatusForbidden)

	sub := f.submitTurn(conv.ID, "hi", "", nil)
	awaitTerminal(t, f.store, owner, sub.Turn.ID)

	// Access is revocable while the conversation exists.
	policy.revoke("site-1")
	missing := f.call(http.MethodGet, testPrefix+"/conversations/"+"000000000000000", nil, nil).expect(http.StatusNotFound)
	gone := f.call(http.MethodGet, testPrefix+"/conversations/"+conv.ID, nil, nil).expect(http.StatusNotFound)
	if !bytes.Equal(gone.body, missing.body) {
		t.Fatalf("revoked target body %s differs from missing-record body %s", gone.body, missing.body)
	}
	f.call(http.MethodPost, testPrefix+"/conversations/"+conv.ID+"/turns", map[string]any{"text": "hi"}, nil).
		expect(http.StatusNotFound)
	f.call(http.MethodGet, testPrefix+"/turns/"+sub.Turn.ID, nil, nil).expect(http.StatusNotFound)
	f.call(http.MethodGet, testPrefix+"/turns/"+sub.Turn.ID+"/events", nil, nil).expect(http.StatusNotFound)
	f.call(http.MethodPost, testPrefix+"/turns/"+sub.Turn.ID+"/cancel", nil, nil).expect(http.StatusNotFound)

	hidden := f.call(http.MethodGet, testPrefix+"/conversations", nil, nil).expect(http.StatusOK)
	var hiddenList conversationListView
	hidden.decode(&hiddenList)
	if len(hiddenList.Items) != 0 {
		t.Fatalf("unfiltered list still shows %d revoked conversations", len(hiddenList.Items))
	}

	for _, user := range policy.checkedUsers() {
		if user != "user-a" {
			t.Fatalf("TargetCheck saw owner %q, want the authenticated caller", user)
		}
	}
}
