package pocketbase

// ProviderStore is the durable catalog.Store for the stock adapter. It keeps
// provider configuration, encrypted API keys, codex credentials and small
// catalog settings in three private base collections provisioned with the same
// prefix as the chat store.
//
// Secrets are encrypted with PocketBase's own security.Encrypt using the app's
// configured encryption env (normally PB_ENCRYPTION_KEY). If no key is
// configured the store refuses to save a secret instead of writing plaintext.
//
// Rows are scoped by (namespace, mount), so several apps can share one fork
// database without seeing each other's providers.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/security"

	"github.com/compdani/pocketbase-mcp/chat"
	"github.com/compdani/pocketbase-mcp/chat/provider/catalog"
	"github.com/compdani/pocketbase-mcp/chat/provider/codex"
)

const (
	providerFieldNamespace     = "namespace"
	providerFieldMount         = "mount"
	providerFieldSlug          = "slug"
	providerFieldLabel         = "label"
	providerFieldKind          = "kind"
	providerFieldEnabled       = "enabled"
	providerFieldBaseURL       = "baseURL"
	providerFieldOAuthURL      = "oauthBaseURL"
	providerFieldWireAPI       = "wireAPI"
	providerFieldMaxTokens     = "maxTokensField"
	providerFieldModels        = "models"
	providerFieldDefault       = "defaultModel"
	providerFieldHeaders       = "headers" // deprecated plaintext field, read-only fallback
	providerFieldHeadersCipher = "headersCipher"
	providerFieldExtraBody     = "extraBody"
	providerFieldAPIKey        = "apiKeyCipher"
	providerFieldAPIKeyHint    = "apiKeyHint"
	providerFieldCredentials   = "credentials"
	providerFieldKey           = "key"
	providerFieldValue         = "value"
	providerFieldCreatedAt     = "createdAt"
	providerFieldUpdatedAt     = "updatedAt"
)

// ProviderStoreOptions configures NewProviderStore.
type ProviderStoreOptions struct {
	Prefix string
	Scope  catalog.Scope
}

// ProviderStore persists provider configuration for one catalog scope.
type ProviderStore struct {
	app       core.App
	scope     catalog.Scope
	providers string
	auth      string
	settings  string
}

var _ catalog.Store = (*ProviderStore)(nil)

// NewProviderStore validates options and idempotently provisions the three
// private collections.
func NewProviderStore(app core.App, opts ProviderStoreOptions) (*ProviderStore, error) {
	if app == nil {
		return nil, errors.New("provider store: nil app")
	}
	if err := opts.Scope.Validate(); err != nil {
		return nil, err
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultChatCollectionPrefix
	}
	if !chatCollectionPrefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("provider store: collection prefix %q must match %s", prefix, chatCollectionPrefixPattern)
	}
	s := &ProviderStore{
		app:       app,
		scope:     opts.Scope,
		providers: prefix + "Providers",
		auth:      prefix + "ProviderAuth",
		settings:  prefix + "Settings",
	}
	for _, name := range s.Collections() {
		if len(name) > chatCollectionNameMaxLen {
			return nil, fmt.Errorf("provider store: collection name %q exceeds %d characters; use a shorter prefix", name, chatCollectionNameMaxLen)
		}
	}
	if err := app.RunInTransaction(func(tx core.App) error {
		for _, spec := range s.specs() {
			if _, err := provisionChatCollection(tx, spec); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// Collections returns the provisioned collection names.
func (s *ProviderStore) Collections() []string {
	return []string{s.providers, s.auth, s.settings}
}

// --- provisioning ---

func (s *ProviderStore) specs() []chatCollectionSpec {
	scopeFields := func() []core.Field {
		return []core.Field{
			&core.TextField{Name: providerFieldNamespace},
			&core.TextField{Name: providerFieldMount},
			&core.TextField{Name: providerFieldSlug},
		}
	}
	return []chatCollectionSpec{
		{
			name: s.providers,
			fields: func() []core.Field {
				fields := scopeFields()
				return append(fields,
					&core.TextField{Name: providerFieldLabel},
					&core.TextField{Name: providerFieldKind},
					&core.BoolField{Name: providerFieldEnabled},
					&core.TextField{Name: providerFieldBaseURL},
					&core.TextField{Name: providerFieldOAuthURL},
					&core.TextField{Name: providerFieldWireAPI},
					&core.TextField{Name: providerFieldMaxTokens},
					&core.JSONField{Name: providerFieldModels, MaxSize: 64 << 10},
					&core.TextField{Name: providerFieldDefault},
					&core.TextField{Name: providerFieldHeadersCipher, Max: 64 << 10},
					// Deprecated plaintext field, kept empty; read only as a
					// migration fallback for early development data.
					&core.JSONField{Name: providerFieldHeaders, MaxSize: 16 << 10},
					&core.JSONField{Name: providerFieldExtraBody, MaxSize: 16 << 10},
					&core.TextField{Name: providerFieldAPIKey, Max: 64 << 10},
					&core.TextField{Name: providerFieldAPIKeyHint},
					&core.DateField{Name: providerFieldCreatedAt},
					&core.DateField{Name: providerFieldUpdatedAt},
				)
			},
			// The cipher field was added after the first provider-store shape;
			// an existing collection gains it in place.
			addFields: []string{providerFieldHeadersCipher, providerFieldMaxTokens},
			indexes: []chatIndexSpec{
				{name: "idx_" + s.providers + "_scope_slug", unique: true, columns: providerFieldNamespace + ", " + providerFieldMount + ", " + providerFieldSlug},
			},
		},
		{
			name: s.auth,
			fields: func() []core.Field {
				fields := scopeFields()
				return append(fields,
					&core.TextField{Name: providerFieldCredentials, Max: 1 << 20},
					&core.DateField{Name: providerFieldUpdatedAt},
				)
			},
			indexes: []chatIndexSpec{
				{name: "idx_" + s.auth + "_scope_slug", unique: true, columns: providerFieldNamespace + ", " + providerFieldMount + ", " + providerFieldSlug},
			},
		},
		{
			name: s.settings,
			fields: func() []core.Field {
				fields := scopeFields()
				return append(fields,
					&core.TextField{Name: providerFieldKey},
					&core.JSONField{Name: providerFieldValue, MaxSize: 64 << 10},
					&core.DateField{Name: providerFieldUpdatedAt},
				)
			},
			indexes: []chatIndexSpec{
				{name: "idx_" + s.settings + "_scope_key", unique: true, columns: providerFieldNamespace + ", " + providerFieldMount + ", " + providerFieldKey},
			},
		},
	}
}

// --- configuration ---

func (s *ProviderStore) scopeParams() dbx.Params {
	return dbx.Params{providerFieldNamespace: s.scope.Namespace, providerFieldMount: s.scope.Mount}
}

func (s *ProviderStore) ListProviders(ctx context.Context, scope catalog.Scope) ([]catalog.Provider, error) {
	if err := s.checkScope(scope); err != nil {
		return nil, err
	}
	recs, err := s.app.FindRecordsByFilter(s.providers,
		providerFieldNamespace+" = {:namespace} && "+providerFieldMount+" = {:mount}",
		providerFieldCreatedAt+",id", 0, 0, s.scopeParams())
	if err != nil {
		return nil, err
	}
	out := make([]catalog.Provider, 0, len(recs))
	for _, rec := range recs {
		p, err := s.providerFrom(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *ProviderStore) GetProvider(ctx context.Context, scope catalog.Scope, id string) (catalog.Provider, error) {
	if err := s.checkScope(scope); err != nil {
		return catalog.Provider{}, err
	}
	params := s.scopeParams()
	params[providerFieldSlug] = id
	rec, err := chatFirst(s.app, s.providers,
		providerFieldNamespace+" = {:namespace} && "+providerFieldMount+" = {:mount} && "+providerFieldSlug+" = {:slug}", params)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			return catalog.Provider{}, catalog.ErrProviderNotFound
		}
		return catalog.Provider{}, err
	}
	return s.providerFrom(rec)
}

func (s *ProviderStore) SaveProvider(ctx context.Context, scope catalog.Scope, p catalog.Provider) error {
	if err := s.checkScope(scope); err != nil {
		return err
	}
	p = p.Normalized()
	if err := p.Validate(); err != nil {
		return err
	}
	return s.app.RunInTransaction(func(tx core.App) error {
		col, err := chatCollection(tx, s.providers)
		if err != nil {
			return err
		}
		params := s.scopeParams()
		params[providerFieldSlug] = p.ID
		rec, err := chatFirst(tx, s.providers,
			providerFieldNamespace+" = {:namespace} && "+providerFieldMount+" = {:mount} && "+providerFieldSlug+" = {:slug}", params)
		if err != nil {
			if !errors.Is(err, chat.ErrNotFound) {
				return err
			}
			rec = core.NewRecord(col)
			rec.Set(providerFieldNamespace, s.scope.Namespace)
			rec.Set(providerFieldMount, s.scope.Mount)
			rec.Set(providerFieldSlug, p.ID)
			rec.Set(providerFieldCreatedAt, chatTime(time.Now()))
		}
		rec.Set(providerFieldLabel, p.Label)
		rec.Set(providerFieldKind, string(p.Kind))
		rec.Set(providerFieldEnabled, p.Enabled)
		rec.Set(providerFieldBaseURL, p.BaseURL)
		rec.Set(providerFieldOAuthURL, p.OAuthBaseURL)
		rec.Set(providerFieldWireAPI, p.WireAPI)
		rec.Set(providerFieldMaxTokens, p.MaxTokensField)
		rec.Set(providerFieldDefault, p.DefaultModel)
		s.setJSON(rec, providerFieldModels, p.Models)
		// Header values can carry credentials, so they are encrypted as one
		// JSON blob rather than stored as plaintext. The old plaintext field
		// is cleared on save.
		headersCipher := ""
		if len(p.Headers) > 0 {
			rawHeaders, err := json.Marshal(p.Headers)
			if err != nil {
				return err
			}
			headersCipher, err = s.encrypt(rawHeaders)
			if err != nil {
				return err
			}
		}
		rec.Set(providerFieldHeadersCipher, headersCipher)
		chatSetJSON(rec, providerFieldHeaders, nil)
		s.setJSON(rec, providerFieldExtraBody, p.ExtraBody)
		cipher, hint := "", ""
		if p.APIKey != "" {
			cipher, err = s.encrypt([]byte(p.APIKey))
			if err != nil {
				return err
			}
			hint = keySuffix(p.APIKey)
		}
		rec.Set(providerFieldAPIKey, cipher)
		rec.Set(providerFieldAPIKeyHint, hint)
		rec.Set(providerFieldUpdatedAt, chatTime(time.Now()))
		if err := tx.Save(rec); err != nil {
			if chatUniqueViolation(err) {
				return catalog.ErrProviderExists
			}
			return err
		}
		return nil
	})
}

func (s *ProviderStore) DeleteProvider(ctx context.Context, scope catalog.Scope, id string) error {
	if err := s.checkScope(scope); err != nil {
		return err
	}
	return s.app.RunInTransaction(func(tx core.App) error {
		params := s.scopeParams()
		params[providerFieldSlug] = id
		rec, err := chatFirst(tx, s.providers,
			providerFieldNamespace+" = {:namespace} && "+providerFieldMount+" = {:mount} && "+providerFieldSlug+" = {:slug}", params)
		if err != nil {
			if errors.Is(err, chat.ErrNotFound) {
				return catalog.ErrProviderNotFound
			}
			return err
		}
		if err := tx.Delete(rec); err != nil {
			return err
		}
		auth, err := chatFirst(tx, s.auth,
			providerFieldNamespace+" = {:namespace} && "+providerFieldMount+" = {:mount} && "+providerFieldSlug+" = {:slug}", params)
		if err == nil {
			if err := tx.Delete(auth); err != nil {
				return err
			}
		} else if !errors.Is(err, chat.ErrNotFound) {
			return err
		}
		return nil
	})
}

// --- codex credentials ---

func (s *ProviderStore) LoadCredentials(ctx context.Context, scope catalog.Scope, id string) (*codex.Credentials, error) {
	if err := s.checkScope(scope); err != nil {
		return nil, err
	}
	params := s.scopeParams()
	params[providerFieldSlug] = id
	rec, err := chatFirst(s.app, s.auth,
		providerFieldNamespace+" = {:namespace} && "+providerFieldMount+" = {:mount} && "+providerFieldSlug+" = {:slug}", params)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			return nil, codex.ErrNotSignedIn
		}
		return nil, err
	}
	cipher := rec.GetString(providerFieldCredentials)
	if cipher == "" {
		return nil, codex.ErrNotSignedIn
	}
	raw, err := s.decrypt(cipher)
	if err != nil {
		return nil, err
	}
	var cred codex.Credentials
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("provider store: decode credentials: %w", err)
	}
	return &cred, nil
}

func (s *ProviderStore) SaveCredentials(ctx context.Context, scope catalog.Scope, id string, cred *codex.Credentials) error {
	if err := s.checkScope(scope); err != nil {
		return err
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	cipher, err := s.encrypt(raw)
	if err != nil {
		return err
	}
	return s.app.RunInTransaction(func(tx core.App) error {
		col, err := chatCollection(tx, s.auth)
		if err != nil {
			return err
		}
		params := s.scopeParams()
		params[providerFieldSlug] = id
		rec, err := chatFirst(tx, s.auth,
			providerFieldNamespace+" = {:namespace} && "+providerFieldMount+" = {:mount} && "+providerFieldSlug+" = {:slug}", params)
		if err != nil {
			if !errors.Is(err, chat.ErrNotFound) {
				return err
			}
			rec = core.NewRecord(col)
			rec.Set(providerFieldNamespace, s.scope.Namespace)
			rec.Set(providerFieldMount, s.scope.Mount)
			rec.Set(providerFieldSlug, id)
		}
		rec.Set(providerFieldCredentials, cipher)
		rec.Set(providerFieldUpdatedAt, chatTime(time.Now()))
		return tx.Save(rec)
	})
}

func (s *ProviderStore) ClearCredentials(ctx context.Context, scope catalog.Scope, id string) error {
	if err := s.checkScope(scope); err != nil {
		return err
	}
	params := s.scopeParams()
	params[providerFieldSlug] = id
	rec, err := chatFirst(s.app, s.auth,
		providerFieldNamespace+" = {:namespace} && "+providerFieldMount+" = {:mount} && "+providerFieldSlug+" = {:slug}", params)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			return nil
		}
		return err
	}
	return s.app.Delete(rec)
}

// --- settings ---

func (s *ProviderStore) LoadSetting(ctx context.Context, scope catalog.Scope, key string, dst any) error {
	if err := s.checkScope(scope); err != nil {
		return err
	}
	params := s.scopeParams()
	params[providerFieldKey] = key
	rec, err := chatFirst(s.app, s.settings,
		providerFieldNamespace+" = {:namespace} && "+providerFieldMount+" = {:mount} && "+providerFieldKey+" = {:key}", params)
	if err != nil {
		if errors.Is(err, chat.ErrNotFound) {
			return catalog.ErrSettingNotFound
		}
		return err
	}
	raw := chatRawJSON(rec, providerFieldValue)
	if len(raw) == 0 {
		return catalog.ErrSettingNotFound
	}
	return json.Unmarshal(raw, dst)
}

func (s *ProviderStore) SaveSetting(ctx context.Context, scope catalog.Scope, key string, value any) error {
	if err := s.checkScope(scope); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.app.RunInTransaction(func(tx core.App) error {
		col, err := chatCollection(tx, s.settings)
		if err != nil {
			return err
		}
		params := s.scopeParams()
		params[providerFieldKey] = key
		rec, err := chatFirst(tx, s.settings,
			providerFieldNamespace+" = {:namespace} && "+providerFieldMount+" = {:mount} && "+providerFieldKey+" = {:key}", params)
		if err != nil {
			if !errors.Is(err, chat.ErrNotFound) {
				return err
			}
			rec = core.NewRecord(col)
			rec.Set(providerFieldNamespace, s.scope.Namespace)
			rec.Set(providerFieldMount, s.scope.Mount)
			rec.Set(providerFieldKey, key)
		}
		chatSetJSON(rec, providerFieldValue, raw)
		rec.Set(providerFieldUpdatedAt, chatTime(time.Now()))
		return tx.Save(rec)
	})
}

func (s *ProviderStore) HostID(ctx context.Context, scope catalog.Scope) (string, error) {
	if err := s.checkScope(scope); err != nil {
		return "", err
	}
	var hostID string
	err := s.LoadSetting(ctx, scope, "hostId", &hostID)
	if err == nil && validHostID(hostID) {
		return hostID, nil
	}
	if err != nil && !errors.Is(err, catalog.ErrSettingNotFound) {
		return "", err
	}
	hostID = newHostID()
	if err := s.SaveSetting(ctx, scope, "hostId", hostID); err != nil {
		// A concurrent first use may have won the unique key. Prefer the
		// stored value over returning a spurious startup failure.
		var existing string
		if loadErr := s.LoadSetting(ctx, scope, "hostId", &existing); loadErr == nil && validHostID(existing) {
			return existing, nil
		}
		return "", err
	}
	return hostID, nil
}

// --- helpers ---

func (s *ProviderStore) checkScope(scope catalog.Scope) error {
	if scope != s.scope {
		return errors.New("provider store: scope does not match this store")
	}
	return scope.Validate()
}

func (s *ProviderStore) providerFrom(rec *core.Record) (catalog.Provider, error) {
	p := catalog.Provider{
		ID:             rec.GetString(providerFieldSlug),
		Label:          rec.GetString(providerFieldLabel),
		Kind:           catalog.Kind(rec.GetString(providerFieldKind)),
		Enabled:        rec.GetBool(providerFieldEnabled),
		BaseURL:        rec.GetString(providerFieldBaseURL),
		OAuthBaseURL:   rec.GetString(providerFieldOAuthURL),
		WireAPI:        rec.GetString(providerFieldWireAPI),
		MaxTokensField: rec.GetString(providerFieldMaxTokens),
		DefaultModel:   rec.GetString(providerFieldDefault),
	}
	if raw := chatRawJSON(rec, providerFieldModels); len(raw) > 0 {
		_ = json.Unmarshal(raw, &p.Models)
	}
	if cipher := rec.GetString(providerFieldHeadersCipher); cipher != "" {
		raw, err := s.decrypt(cipher)
		if err != nil {
			p.SecretError = "The provider headers could not be decrypted; check the encryption key or re-enter them."
			p.HeadersUnreadable = true
		} else {
			_ = json.Unmarshal(raw, &p.Headers)
		}
	} else if raw := chatRawJSON(rec, providerFieldHeaders); len(raw) > 0 {
		// Early development data may still have the plaintext field. It is
		// migrated to the cipher field on the next save.
		_ = json.Unmarshal(raw, &p.Headers)
	}
	if raw := chatRawJSON(rec, providerFieldExtraBody); len(raw) > 0 {
		_ = json.Unmarshal(raw, &p.ExtraBody)
	}
	if cipher := rec.GetString(providerFieldAPIKey); cipher != "" {
		raw, err := s.decrypt(cipher)
		if err != nil {
			p.SecretError = "The API key could not be decrypted; check the encryption key or re-enter it."
			p.APIKeyUnreadable = true
		} else {
			p.APIKey = string(raw)
		}
	}
	return p.Normalized(), nil
}

func (s *ProviderStore) setJSON(rec *core.Record, name string, value any) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 {
		rec.Set(name, nil)
		return
	}
	chatSetJSON(rec, name, raw)
}

func (s *ProviderStore) encryptionKey() (string, error) {
	key := strings.TrimSpace(os.Getenv(s.app.EncryptionEnv()))
	if key == "" {
		return "", catalog.ErrNoEncryptionKey
	}
	return key, nil
}

func (s *ProviderStore) encrypt(raw []byte) (string, error) {
	key, err := s.encryptionKey()
	if err != nil {
		return "", err
	}
	cipher, err := security.Encrypt(raw, key)
	if err != nil {
		return "", fmt.Errorf("provider store: encryption key is invalid: %w", err)
	}
	return cipher, nil
}

func (s *ProviderStore) decrypt(cipher string) ([]byte, error) {
	key, err := s.encryptionKey()
	if err != nil {
		return nil, err
	}
	return security.Decrypt(cipher, key)
}

func keySuffix(key string) string {
	if len(key) <= 4 {
		return "…"
	}
	return "…" + key[len(key)-4:]
}

func newHostID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return "urn:uuid:" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func validHostID(id string) bool {
	return strings.HasPrefix(id, "urn:uuid:") ||
		strings.HasPrefix(id, "urn:ietf:params:oauth:jwk-thumbprint:") ||
		strings.HasPrefix(id, "did:key:")
}
