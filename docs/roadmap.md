# Roadmap

Status on 2026-11-19. Done, committed and covered by tests: the MCP server
(milestones 1–5), chat v1.1 (read-only) and chat v1.2 (proposals, editor
tools, uploads). The fork's OAuth branch is merged (pb-postgress PR #4) and
pinned at `v0.41.6`. Chat v1.3 (provider catalog, OpenAI
Responses, ChatGPT/Codex plan sign-in) is implemented and has had one review
pass. It still needs staging and a real Codex sign-in. v0.1.2 adds a log
line for every failed chat turn (with the provider's sanitized reason), plain
messages for failed-turn codes in the agent-panel binding, and per-turn
reasoning effort.

Unreleased (branch `pocketbase-v0.41`, for the app migrations): the stock
adapter, stockoauth and stock example build against PocketBase v0.41.1 on
`ganigeorgiev.com/pocketbase/pocketbase`; `ResolveCaller` in both adapters
lets a custom tool run the host's own handlers as the caller; turns can carry
a page `context` (`TurnContext`), and `RunnerConfig.Setup` picks each turn's
system prompt and limits from it; `RunnerConfig.MaxOutputTokens`; and
`ToolSpec.Validate` / `ToolSpec.Summarize` for structured editor patches and
other checks a schema can't express.

Suggested order: browser check → staging tests → hardening →
migrations.

## Next

- **Staging-test chat v1.3.** Add a real provider, sign in Codex on a
  staging copy, and run a tool-using turn with an attachment.
- **Decide whether a changed provider host requires the key again.** Today a
  saved key follows the entry's `baseURL`. See the review note in
  [chat-providers-plan.md](chat-providers-plan.md).
- **Try the chat examples in a real browser.** So far they were checked with
  Node scripts, `curl`, Vite builds and serving their index page. Sign in and
  chat once by hand. Nuxt is described in the Vue README but untested.
- **Staging tests with real clients:** ChatGPT with a hand-created client and
  one self-registering client such as Claude Code, on a public URL (see
  [operations.md](operations.md#deployment-checklist-fork)).

## Before release (milestone 6)

- **Not built:** per-user rate limits and a concurrency cap for `/mcp`.
- **Tests still needed:** schema and instance-scope changes while running,
  two apps in one process, clean shutdown, and no tool-list leaks between
  users under load.
- **Release material:** a support matrix (PocketBase versions, fork commit,
  Go version, databases) and a written customjs migration guide.
- **customjs switch-over:** follow
  [operations.md](operations.md#migrating-from-customjs), testing against
  ChatGPT on staging first.
- **Optional:** keep the self-registration counts in the database, for
  several instances without a proxy limit.

## Fork (pb-postgress)

- **MySQL has never been tested** for any of the fork changes, including the
  chat collections, whose uniqueness design was made for MySQL. Run the fork
  tests with `PB_TEST_DB_DRIVER=mysql` (needs a local MySQL or MariaDB).
- **Data race in background request logging.** Async request-log writes race
  with shutdown. This repo's tests work around it; fix it in the fork.
- **Optional:** make shutdown wait for background file deletions. That race
  caused the old flaky `TestNativeDiscoveryRegistrationPKCE`.

## App migrations

What List Pocket still needs from this module, after the work above:

- **Two-factor sign-in for remote MCP.** stockoauth signs in through
  `auth-with-password`, which List Pocket refuses for users with its own TOTP
  and for OIDC-only users. Needs a host-defined second step, or hand-off to
  an existing app session.
- **A stdio entry point** (a small CLI that talks to `/mcp`), so existing
  stdio client setups such as `listpocket-mcp` keep working.
- **History import** for conversations kept in the browser today.
- **Shared provider settings:** a `catalog.Store` backed by the app's own AI
  settings, so it doesn't keep two keys and two ChatGPT sign-ins.
  `ChatConfig.ProviderStore` is the hook; no example exists yet.

Move one screen each in List Pocket, Event Pocket and Static Dealmakers onto
the chat module, as separate work. Keep each app's permission policy,
prompts, tool coverage, conversation ownership and unsaved-editor behaviour;
import old history explicitly and keep the original data until it's
verified. The current assistants in those apps stay as they are until then.

## Known gaps

Chat:

- **Provider settings UI.** The superuser provider API is implemented; the
  plain example does not yet ship a settings panel.
- **Live Codex sign-in.** The OAuth flow is covered by a mock authorization
  server; it still needs a real ChatGPT plan sign-in on staging.
- **Upload count limit is approximate.** The per-conversation limit is
  checked before a file is saved, so simultaneous uploads can go slightly
  over. The per-message limit is exact.
- **Target not re-checked during a turn.** A conversation's target is checked
  when a message is sent, not again while the turn reads its attachments.
- **Field values of generated writes are checked late.** A proposal's shape is
  checked when it's made, but the values inside `data` are only validated by
  PocketBase when the approved change runs.
- **Provider request size** is capped at 32 MiB for attachments. A provider
  or gateway with a smaller limit fails the turn.
- **Single instance.** Chat runs in one process per namespace and mount.

Code:

- **Adapter duplication.** The stock and fork adapters' chat code is nearly
  identical, and so is the chat setup in the two examples. They're separate
  Go modules built against different PocketBase modules, so they can't share
  code; fix bugs in both.
- **Hand-written typings.** The browser client's `.d.mts` files are written
  by hand; a test keeps their exports in step with the code.

## Later

- stdio or REST sidecar
- MCP resources and prompts
- file upload tools
- idempotency keys for writes
- multi-instance chat
- maybe Client ID Metadata Documents as an alternative to self-registration
