package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// refreshAhead refreshes an access token before it expires, so a long agent
	// turn does not start on a token that dies mid-flight.
	refreshAhead = 10 * time.Minute
	// signInTTL bounds one authorization attempt.
	signInTTL = 10 * time.Minute
)

// Manager owns one provider's credentials, single-flight refresh and sign-in
// state. The catalog keeps one manager per provider id across reloads, so two
// provider instances can never refresh the same rotating token twice.
type Manager struct {
	Store      Store
	OAuth      *OAuth
	HostID     string
	ProviderID string
	Now        func() time.Time

	refreshMu sync.Mutex
	mu        sync.Mutex
	pending   *pendingSignIn
	lastErr   string
	closed    bool
}

type pendingSignIn struct {
	adminID   string
	pending   *Pending
	loopback  *Loopback
	expiresAt time.Time
	done      bool
}

// NewManager returns a manager for one provider.
func NewManager(store Store, oauth *OAuth, hostID, providerID string) *Manager {
	if oauth == nil {
		oauth = &OAuth{}
	}
	return &Manager{Store: store, OAuth: oauth, HostID: hostID, ProviderID: providerID}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Status summarizes the account and any pending sign-in. It never includes a
// token.
func (m *Manager) Status(ctx context.Context) Status {
	status := Status{HostID: m.HostID}
	m.mu.Lock()
	if m.pending != nil && !m.pending.done && m.now().Before(m.pending.expiresAt) {
		status.Pending = true
	}
	status.Error = m.lastErr
	m.mu.Unlock()

	creds, err := m.Store.Load(ctx)
	if err != nil {
		if !errors.Is(err, ErrNotSignedIn) {
			status.Error = "The sign-in state could not be read."
		}
		return status
	}
	status.SignedIn = true
	status.Email = creds.Email
	status.Plan = creds.HasPlan()
	status.ExpiresAt = creds.ExpiresAt
	status.ClientID = creds.ClientID
	return status
}

// Token returns an access token valid for at least refreshAhead. forceRefresh
// renews even when the cached token is still valid; the provider uses it once
// after a 401.
func (m *Manager) Token(ctx context.Context, forceRefresh bool) (string, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return "", errors.New("codex: provider was removed")
	}
	creds, err := m.Store.Load(ctx)
	if err != nil {
		if errors.Is(err, ErrNotSignedIn) {
			return "", ErrNotSignedIn
		}
		return "", fmt.Errorf("codex: load credentials: %w", err)
	}
	if !creds.HasPlan() {
		return "", ErrPlanNotGranted
	}
	if !forceRefresh && creds.AccessToken != "" && creds.ExpiresAt.Sub(m.now()) > refreshAhead {
		return creds.AccessToken, nil
	}
	if creds.RefreshToken == "" {
		return "", errors.New("codex: the ChatGPT session expired; sign in again")
	}
	next, err := m.OAuth.Refresh(ctx, creds)
	if err != nil {
		return "", fmt.Errorf("codex: refresh failed; sign in again: %w", err)
	}
	if next.HostID == "" {
		next.HostID = m.HostID
	}
	// A reload may have removed the provider while the refresh was in flight.
	// Do not recreate its credential row.
	m.mu.Lock()
	closed = m.closed
	m.mu.Unlock()
	if closed {
		return "", errors.New("codex: provider was removed")
	}
	if err := m.Store.Save(ctx, next); err != nil {
		return "", fmt.Errorf("codex: save refreshed credentials: %w", err)
	}
	return next.AccessToken, nil
}

// BeginSignIn starts one authorization attempt and returns the URL the admin's
// browser should open. A loopback listener is started when possible; a remote
// browser falls back to CompleteSignIn with the pasted redirect URL.
func (m *Manager) BeginSignIn(ctx context.Context, adminID string) (SignInStart, error) {
	existing, _ := m.Store.Load(ctx)

	m.mu.Lock()
	if m.pending != nil && !m.pending.done && m.now().Before(m.pending.expiresAt) {
		m.mu.Unlock()
		return SignInStart{}, errors.New("a sign-in is already in progress")
	}
	if m.pending != nil && m.pending.loopback != nil {
		m.pending.loopback.Close()
	}
	m.pending = nil
	m.lastErr = ""
	m.mu.Unlock()

	loopback, err := Listen(0)
	redirectURI := "http://127.0.0.1:" + fmt.Sprint(DefaultPort) + CallbackPath
	if err == nil {
		redirectURI = loopback.RedirectURI
	}
	p := m.OAuth.Begin(m.HostID, redirectURI, existing)
	expiresAt := m.now().Add(signInTTL)

	attempt := &pendingSignIn{
		adminID:   adminID,
		pending:   p,
		loopback:  loopback,
		expiresAt: expiresAt,
	}
	m.mu.Lock()
	m.pending = attempt
	m.mu.Unlock()

	if loopback != nil {
		go func() {
			waitCtx, cancel := context.WithTimeout(context.Background(), signInTTL)
			defer cancel()
			query, err := loopback.Wait(waitCtx)
			if err != nil {
				// Only fail this attempt. A pasted completion, a cancel or a
				// newer attempt may already have replaced it.
				m.failAttempt(attempt, err)
				return
			}
			_ = m.completeSignIn(context.Background(), adminID, query)
		}()
	}
	return SignInStart{AuthURL: p.AuthURL, ExpiresAt: expiresAt}, nil
}

// CompleteSignIn finishes a sign-in from a pasted redirect URL. It is also the
// path a loopback callback uses internally, so it is idempotent for one
// attempt.
func (m *Manager) CompleteSignIn(ctx context.Context, adminID, callbackURL string) error {
	query, err := callbackQuery(callbackURL)
	if err != nil {
		return err
	}
	return m.completeSignIn(ctx, adminID, query)
}

func callbackQuery(callbackURL string) (url.Values, error) {
	raw := strings.TrimSpace(callbackURL)
	if raw == "" {
		return nil, errors.New("paste the full redirect address: it contains code= and state=")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("paste the full redirect address: it contains code= and state=")
	}
	q := u.Query()
	if len(q) == 0 {
		q, err = url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if err != nil {
			return nil, errors.New("paste the full redirect address: it contains code= and state=")
		}
	}
	if q.Get("state") == "" {
		return nil, errors.New("paste the full redirect address: it contains code= and state=")
	}
	return q, nil
}

func (m *Manager) completeSignIn(ctx context.Context, adminID string, query url.Values) error {
	m.mu.Lock()
	pending := m.pending
	switch {
	case pending == nil:
		m.mu.Unlock()
		return errors.New("no sign-in is in progress; start again")
	case pending.done:
		m.mu.Unlock()
		return errors.New("this sign-in has already finished")
	case pending.adminID != adminID:
		m.mu.Unlock()
		return errors.New("this sign-in belongs to another administrator")
	case !m.now().Before(pending.expiresAt):
		m.mu.Unlock()
		m.failSignIn(adminID, errors.New("sign-in timed out; start again"))
		return errors.New("sign-in timed out; start again")
	}
	pending.done = true
	p := pending.pending
	loopback := pending.loopback
	m.mu.Unlock()

	cred, err := m.OAuth.Complete(ctx, p, query)
	if err != nil {
		m.failSignIn(adminID, err)
		return err
	}
	if !cred.HasPlan() {
		m.failSignIn(adminID, ErrPlanNotGranted)
		return ErrPlanNotGranted
	}
	cred.HostID = m.HostID
	if err := m.Store.Save(ctx, cred); err != nil {
		m.failSignIn(adminID, fmt.Errorf("save credentials: %w", err))
		return fmt.Errorf("save credentials: %w", err)
	}

	m.mu.Lock()
	m.pending = nil
	m.lastErr = ""
	m.mu.Unlock()
	loopback.Close()
	return nil
}

// CancelSignIn abandons one attempt so a closed tab does not block another.
func (m *Manager) CancelSignIn(_ context.Context, adminID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		return nil
	}
	if m.pending.adminID != adminID {
		return errors.New("this sign-in belongs to another administrator")
	}
	m.pending.done = true
	if m.pending.loopback != nil {
		m.pending.loopback.Close()
	}
	m.pending = nil
	return nil
}

// SignOut revokes the refresh token and forgets the account locally. A
// revocation failure is reported after the local clear so a stale local token
// never keeps working.
func (m *Manager) SignOut(ctx context.Context) error {
	creds, err := m.Store.Load(ctx)
	var revokeErr error
	if err == nil {
		revokeErr = m.OAuth.Revoke(ctx, creds)
	}
	if err := m.Store.Clear(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	if m.pending != nil && m.pending.loopback != nil {
		m.pending.loopback.Close()
	}
	m.pending = nil
	m.lastErr = ""
	m.mu.Unlock()
	return revokeErr
}

// Import stores credentials exported from another install. The import keeps
// this install's own host id for future authorizations.
func (m *Manager) Import(ctx context.Context, cred *Credentials) error {
	if err := cred.ValidForImport(); err != nil {
		return err
	}
	cred.HostID = m.HostID
	return m.Store.Save(ctx, cred)
}

// Close abandons any pending sign-in and closes its loopback listener. It does
// not change stored credentials.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.pending != nil {
		m.pending.done = true
		if m.pending.loopback != nil {
			m.pending.loopback.Close()
		}
		m.pending = nil
	}
}

func (m *Manager) failSignIn(adminID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil || m.pending.adminID != adminID {
		return
	}
	m.failLocked(err)
}

// failAttempt fails one specific attempt, and nothing if it was replaced.
func (m *Manager) failAttempt(attempt *pendingSignIn, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending != attempt {
		return
	}
	m.failLocked(err)
}

func (m *Manager) failLocked(err error) {
	m.lastErr = err.Error()
	m.pending.done = true
	if m.pending.loopback != nil {
		m.pending.loopback.Close()
	}
	m.pending = nil
}

// DecodeCredentials parses an admin import body. It is a small helper so the
// catalog does not need to know the credential field names.
func DecodeCredentials(raw json.RawMessage) (*Credentials, error) {
	var cred Credentials
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, errors.New("credentials are not valid JSON")
	}
	return &cred, nil
}
