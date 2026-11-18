package pocketbase

// Optional chat wiring for the stock adapter.
//
// Chat is disabled by default: nothing is provisioned and no route is mounted
// unless the host calls Server.EnableChat during its OnServe hook, before
// RegisterRoutes. The mount reuses the adapter as pbmcp.Host, so a chat turn
// authenticates with the same bearer token as /mcp and every tool call is
// revalidated against that identity before it runs.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/router"

	pbmcp "github.com/compdani/pocketbase-mcp"
	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider"
	"github.com/compdani/pocketbase-mcp/chat/provider/catalog"
)

// chatShutdownTimeout bounds how long process termination waits for running
// turns to record their interrupted terminal event.
const chatShutdownTimeout = 5 * time.Second

// ChatConfig configures EnableChat.
//
// Options carries the neutral chat surface settings (namespace, prefix, policy,
// limits, target checks). Its Store and Runner fields may be left nil: the
// adapter then provisions a ChatStore over its own PocketBase app and builds a
// chat.Runner from Provider, Model, System and RunnerConfig. Supplying them is
// how a host plugs in a custom store or a runner it manages itself.
type ChatConfig struct {
	Options pbmcp.ChatOptions
	// Provider is the legacy single provider. It is required only when no
	// catalog mode is configured and no custom Runner is supplied.
	Provider provider.Provider
	// Model is the legacy default model. Ignored in catalog modes.
	Model string
	// System is the system prompt sent with every provider request.
	System string
	// Providers is a static catalog used when ManageProviders is false. It is
	// also the first-boot seed when ManageProviders is true and the database
	// has no provider rows yet.
	Providers []catalog.Provider
	// ManageProviders provisions the private provider collections, loads the
	// catalog from them, and mounts the superuser provider-admin routes.
	ManageProviders bool
	// ProviderStore overrides the adapter's provider store. It is responsible
	// for its own collections when supplied.
	ProviderStore catalog.Store
	// RunnerConfig supplies optional runner limits (steps, timeout, worker
	// bounds, history). Its Store, Provider, Model and System fields are
	// overridden by this config. A custom Options.Runner that resolves a
	// catalog must also receive Options.Router; the adapter cannot infer it.
	RunnerConfig chat.RunnerConfig
	// CollectionPrefix namespaces the private chat collections. Empty means
	// pocketbase.DefaultChatCollectionPrefix.
	CollectionPrefix string
	// ProposalTTL bounds how long a stored proposal may be decided. Zero means
	// the chat default (24 hours). It only applies to the runner this config
	// builds, and it requires Options.Proposals.
	ProposalTTL time.Duration
}

// chatMount is the registered chat surface: its HTTP handler and the runner
// whose lifetime is tied to the app.
type chatMount struct {
	prefix  string
	handler http.Handler
	runner  *chat.Runner
	store   chat.Store
	router  *catalog.Router
	// stopSweep cancels the proposal expiry sweep; sweepDone closes once the
	// sweep goroutine has exited, so shutdown can wait for it.
	stopSweep context.CancelFunc
	sweepDone chan struct{}
}

// EnableChat turns the chat surface on for this server. Call it during OnServe
// before RegisterRoutes: RegisterRoutes mounts the handler when chat is
// enabled.
//
// It provisions (or adopts, idempotently) the private chat collections, runs
// the store's restart recovery once so a turn left non-terminal by a previous
// process ends as interrupted instead of hanging forever, turns a proposal left
// executing by that process into unknown, builds the runner and handler, starts
// the background expiry sweep, and registers an OnTerminate hook that stops the
// sweep and shuts the runner down with a bounded context. Chat stays off —
// nothing provisioned, nothing mounted — when EnableChat is never called.
//
// When Options.Uploads is set, the runner this config builds is given the same
// store as its upload store, so the attachments the handler accepts are the
// ones turns can send. The optional ChatOptions upload limits are passed through
// to the handler unchanged.
func (s *Server) EnableChat(cfg ChatConfig) error {
	if s.chat != nil {
		return errors.New("pocketbase: chat is already enabled for this server")
	}
	mountPath := s.Config().Path
	if mountPath == "" {
		mountPath = "/mcp"
	}
	opts := cfg.Options
	prefix := opts.Prefix
	if prefix == "" {
		prefix = pbmcp.DefaultChatPrefix
	}
	opts.Prefix = prefix

	catalogMode := cfg.ManageProviders || len(cfg.Providers) > 0
	if opts.Runner != nil && catalogMode {
		return errors.New("pocketbase: a custom chat runner cannot be combined with a provider catalog")
	}
	if cfg.Provider != nil && catalogMode {
		return errors.New("pocketbase: set either Provider or Providers/ManageProviders, not both")
	}
	if opts.Runner == nil && !catalogMode && cfg.Provider == nil {
		return errors.New("pocketbase: chat requires a provider, a provider catalog, or a custom runner")
	}
	if catalogMode && strings.TrimSpace(opts.Namespace) == "" {
		return errors.New("pocketbase: a provider catalog requires a chat namespace")
	}

	store := opts.Store
	if store == nil {
		built, err := NewChatStore(s.Adapter.app, ChatStoreOptions{Prefix: cfg.CollectionPrefix})
		if err != nil {
			return fmt.Errorf("pocketbase: chat store: %w", err)
		}
		store = built
	}
	// Restart recovery: a turn that was running when the previous process
	// stopped can never finish, so it ends now with exactly one terminal event.
	if n, err := store.InterruptRunning(context.Background()); err != nil {
		return fmt.Errorf("pocketbase: chat restart recovery: %w", err)
	} else if n > 0 {
		s.Adapter.app.Logger().Info("MCP chat: interrupted turns from a previous process", "turns", n)
	}
	// A proposal that was executing when the previous process stopped may or
	// may not have applied its effect, so it becomes unknown instead of staying
	// undecided forever. This runs whether or not proposals are enabled now:
	// the row is already there and must not stay in limbo.
	proposals, _ := store.(chat.ProposalStore)
	if proposals != nil {
		if n, err := proposals.RecoverExecuting(context.Background()); err != nil {
			return fmt.Errorf("pocketbase: chat proposal recovery: %w", err)
		} else if n > 0 {
			s.Adapter.app.Logger().Info("MCP chat: proposals have an unknown outcome after a restart; check before retrying", "proposals", n)
		}
	}

	var (
		router        *catalog.Router
		providerAdmin chat.ProviderAdmin
		adminAuth     chat.Authenticator
	)
	if catalogMode {
		scope := catalog.Scope{Namespace: opts.Namespace, Mount: mountPath}
		providerStore := cfg.ProviderStore
		if providerStore == nil && cfg.ManageProviders {
			built, err := NewProviderStore(s.Adapter.app, ProviderStoreOptions{Prefix: cfg.CollectionPrefix, Scope: scope})
			if err != nil {
				return fmt.Errorf("pocketbase: provider store: %w", err)
			}
			providerStore = built
		}
		if cfg.ManageProviders && len(cfg.Providers) > 0 {
			existing, err := providerStore.ListProviders(context.Background(), scope)
			if err != nil {
				return fmt.Errorf("pocketbase: provider seed check: %w", err)
			}
			if len(existing) == 0 {
				for _, p := range cfg.Providers {
					if err := providerStore.SaveProvider(context.Background(), scope, p); err != nil {
						return fmt.Errorf("pocketbase: provider seed %q: %w", p.ID, err)
					}
				}
				s.Adapter.app.Logger().Info("MCP chat: seeded providers from config", "providers", len(cfg.Providers))
			}
		}
		built, err := catalog.NewRouter(context.Background(), catalog.RouterOptions{
			Scope:  scope,
			Store:  providerStore,
			Static: staticProviders(cfg),
			Logger: s.Adapter.app.Logger(),
		})
		if err != nil {
			return fmt.Errorf("pocketbase: provider catalog: %w", err)
		}
		router = built
		if cfg.ManageProviders {
			admin, err := catalog.NewAdminWithRouter(router, catalog.RouterOptions{
				Scope:  scope,
				Store:  providerStore,
				Logger: s.Adapter.app.Logger(),
			})
			if err != nil {
				router.Close()
				return fmt.Errorf("pocketbase: provider admin: %w", err)
			}
			providerAdmin = admin
			adminAuth = s.Server.ChatAdminAuthenticator(opts.Namespace)
		}
	}

	runner := opts.Runner
	if runner == nil {
		cfg.RunnerConfig.Store = store
		cfg.RunnerConfig.Provider = cfg.Provider
		cfg.RunnerConfig.Model = cfg.Model
		if router != nil {
			cfg.RunnerConfig.Router = router
		}
		cfg.RunnerConfig.System = cfg.System
		if opts.Proposals && cfg.RunnerConfig.Proposals == nil {
			cfg.RunnerConfig.Proposals = proposals
		}
		if cfg.ProposalTTL > 0 && cfg.RunnerConfig.ProposalTTL <= 0 {
			cfg.RunnerConfig.ProposalTTL = cfg.ProposalTTL
		}
		if opts.Uploads {
			// The handler stores attachments in this store and the runner has
			// to read them back, so both halves must be the same store. A host
			// that asked for uploads but supplied a store without the port is a
			// configuration error, never a silently text-only surface.
			uploads, _ := store.(chat.UploadStore)
			if uploads == nil {
				return errors.New("pocketbase: chat uploads require a store that implements chat.UploadStore")
			}
			if cfg.RunnerConfig.Uploads == nil {
				cfg.RunnerConfig.Uploads = uploads
			}
		}
		built, err := chat.NewRunner(cfg.RunnerConfig)
		if err != nil {
			return fmt.Errorf("pocketbase: chat runner: %w", err)
		}
		runner = built
	}
	opts.Store = store
	opts.Runner = runner
	if router != nil {
		opts.Router = router
	}
	opts.ProviderAdmin = providerAdmin
	opts.AdminAuth = adminAuth
	if len(opts.Models) == 0 && router == nil && strings.TrimSpace(cfg.Model) != "" {
		opts.Models = []string{cfg.Model}
	}

	handler, err := s.Server.ChatHandler(opts)
	if err != nil {
		if router != nil {
			router.Close()
		}
		return fmt.Errorf("pocketbase: chat handler: %w", err)
	}
	mount := &chatMount{prefix: prefix, handler: handler, runner: runner, store: store, router: router}
	s.chat = mount

	// Expired proposals are ended in the background: no client is required to
	// make a deadline effective. The sweep stops with the app so a test (or a
	// restarted process) never leaves a goroutine behind.
	if proposals != nil {
		sweepCtx, stopSweep := context.WithCancel(context.Background())
		mount.stopSweep = stopSweep
		mount.sweepDone = make(chan struct{})
		go s.sweepExpiredProposals(sweepCtx, proposals, mount.sweepDone)
	}

	// Running turns hold a caller token in memory and may still write; give
	// them a bounded chance to store their interrupted terminal event.
	s.Adapter.app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: "pbmcp:" + mountPath + ":chat:shutdown",
		Func: func(e *core.TerminateEvent) error {
			stopChatSweep(mount)
			ctx, cancel := context.WithTimeout(context.Background(), chatShutdownTimeout)
			defer cancel()
			if err := runner.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
				e.App.Logger().Warn("MCP chat: runner shutdown did not finish in time", "error", err)
			}
			if mount.router != nil {
				mount.router.Close()
			}
			return e.Next()
		},
	})
	logArgs := []any{"prefix", prefix, "mount", mountPath, "uploads", opts.Uploads}
	if router != nil {
		logArgs = append(logArgs, "providers", true, "managed", cfg.ManageProviders)
	} else {
		logArgs = append(logArgs, "model", cfg.Model)
	}
	s.Adapter.app.Logger().Info("MCP chat enabled", logArgs...)
	return nil
}

// staticProviders returns the static source for the router. In managed mode
// cfg.Providers is only a first-boot seed, so the database is authoritative.
func staticProviders(cfg ChatConfig) []catalog.Provider {
	if cfg.ManageProviders {
		return nil
	}
	return cfg.Providers
}

// chatExpiryInterval is how often the background sweep ends overdue proposals.
const chatExpiryInterval = time.Minute

// sweepExpiredProposals ends every proposal whose deadline passed, once at
// start and then every chatExpiryInterval, until ctx is cancelled. Failures are
// logged without any proposal content and never stop the sweep.
func (s *Server) sweepExpiredProposals(ctx context.Context, store chat.ProposalStore, done chan<- struct{}) {
	defer close(done)
	s.expireProposals(ctx, store)
	ticker := time.NewTicker(chatExpiryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.expireProposals(ctx, store)
		}
	}
}

func (s *Server) expireProposals(ctx context.Context, store chat.ProposalStore) {
	n, err := store.ExpireProposals(ctx, time.Now().UTC())
	if err != nil {
		if ctx.Err() == nil {
			s.Adapter.app.Logger().Warn("MCP chat: proposal expiry sweep failed", "error", err)
		}
		return
	}
	if n > 0 {
		s.Adapter.app.Logger().Info("MCP chat: expired proposals", "proposals", n)
	}
}

// stopChatSweep cancels the expiry sweep and waits briefly for it to exit.
// Termination must never hang on a sweep that is inside a database call.
func stopChatSweep(mount *chatMount) {
	if mount.stopSweep == nil {
		return
	}
	mount.stopSweep()
	if mount.sweepDone == nil {
		return
	}
	select {
	case <-mount.sweepDone:
	case <-time.After(time.Second):
	}
}

// ChatStorage returns the mounted chat store and reports whether chat is
// enabled. Hosts use it for diagnostics; it is nil until EnableChat succeeds.
func (s *Server) ChatStorage() (chat.Store, bool) {
	if s.chat == nil || s.chat.store == nil {
		return nil, false
	}
	return s.chat.store, true
}

// registerChatRoutes mounts the chat handler under its prefix. The PocketBase
// router only has to dispatch the subtree; the handler owns its own routing and
// authentication, reading the bearer token from the Authorization header the
// same way /mcp does.
//
// Every method is registered because the surface uses GET, POST, PATCH and
// DELETE; the bare prefix is mounted too so the handler answers it with its own
// JSON not-found body instead of the router's.
func (s *Server) registerChatRoutes(r *router.Router[*core.RequestEvent]) error {
	if s.chat == nil {
		return nil
	}
	for _, path := range []string{s.chat.prefix, s.chat.prefix + "/"} {
		if r.HasRoute("", path) {
			return fmt.Errorf("pocketbase: chat route %q conflicts with an existing route", path)
		}
	}
	for _, path := range []string{s.chat.prefix, s.chat.prefix + "/"} {
		r.Route("", path, s.handleChat)
	}
	return nil
}

// handleChat hands the request to the chat handler unchanged.
//
// e.Response is the router's ResponseWriter, which implements http.Flusher and
// http.ResponseWriter.Unwrap, so the chat handler's Server-Sent Events frames
// are flushed through PocketBase instead of being buffered. PocketBase's own
// middlewares only load e.Auth from the token and never reject or rewrite it,
// and the chat handler performs its own authentication (it also revalidates
// before each tool call).
func (s *Server) handleChat(e *core.RequestEvent) error {
	s.chat.handler.ServeHTTP(e.Response, e.Request)
	return nil
}
