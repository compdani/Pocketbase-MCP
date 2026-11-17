package stockoauth

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
)

const txCookie = "mcp_oauth_tx"

var defaultLoginTemplate = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in</title><style>body{font:15px/1.5 system-ui,sans-serif;background:#f4f6f8;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}main{background:#fff;border:1px solid #d6dce1;border-radius:12px;padding:28px 24px;max-width:380px;width:100%}label{display:block;font-weight:600;font-size:13px;margin:12px 0 4px}input{width:100%;box-sizing:border-box;padding:10px;border:1px solid #d6dce1;border-radius:8px}button{width:100%;margin-top:16px;padding:11px;border:0;border-radius:8px;background:#16161a;color:#fff;font-weight:600;cursor:pointer}.error{background:#fde8ec;color:#b4233a;padding:10px 12px;border-radius:8px}</style></head>
<body><main><h1>Sign in</h1><p>to continue to <strong>{{.ClientName}}</strong>{{if .Unverified}} (unverified app){{end}}</p>
{{if .Unverified}}<div class="error" role="note">This app registered itself and its name is not verified. Signing in gives it full access to your account. Only continue if you started this connection yourself.</div>{{end}}
<p>You will be returned to <strong>{{.RedirectHost}}</strong>.</p>
{{if .Error}}<div class="error" role="alert">{{.Error}}</div>{{end}}
<form method="post" action="{{.LoginPath}}">
<input type="hidden" name="csrf_token" value="{{.CSRF}}">
<label for="identity">Email</label><input id="identity" name="identity" value="{{.Identity}}" autocomplete="username" required>
<label for="password">Password</label><input id="password" name="password" type="password" autocomplete="current-password" required>
<button type="submit">Sign in</button></form></main></body></html>`

type loginPageData struct {
	ClientName string
	CSRF       string
	Error      string
	Identity   string
	LoginPath  string
	// RedirectHost is where the authorization code will be sent.
	RedirectHost string
	// Unverified marks a self-registered client: its name is chosen by
	// whoever registered it and must not be trusted.
	Unverified bool
}

// pageClient fills the client-identity fields of the login page.
func pageClient(client *clientRecord, redirectURI string, data loginPageData) loginPageData {
	if u, err := url.Parse(redirectURI); err == nil {
		data.RedirectHost = u.Host
	}
	data.Unverified = client.SelfRegistered
	return data
}

func (p *Provider) renderLogin(e *core.RequestEvent, status int, data loginPageData) error {
	h := e.Response.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	tmplSrc := p.cfg.LoginTemplate
	if tmplSrc == "" {
		tmplSrc = defaultLoginTemplate
	}
	tmpl, err := template.New("login").Parse(tmplSrc)
	if err != nil {
		return apis.NewInternalServerError("Failed to render login page", err)
	}
	data.LoginPath = p.cfg.LoginPath
	e.Response.Header().Set("Cache-Control", "no-store")
	e.Response.Header().Set("X-Frame-Options", "DENY")
	e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
	e.Response.WriteHeader(status)
	return tmpl.Execute(e.Response, data)
}

func oauthError(e *core.RequestEvent, status int, code string, desc string) error {
	e.Response.Header().Set("Cache-Control", "no-store")
	return e.JSON(status, map[string]string{"error": code, "error_description": desc})
}

func (p *Provider) cleanupExpired() {
	now := time.Now().UTC()
	for _, name := range []string{p.cfg.TransactionsCollection(), p.cfg.CodesCollection()} {
		recs, err := p.app.FindRecordsByFilter(name, "expires_at < {:now}", "", 100, 0, map[string]any{"now": now.Format("2006-01-02 15:04:05.000Z")})
		if err != nil {
			continue
		}
		for _, r := range recs {
			_ = p.app.Delete(r)
		}
	}
}

func (p *Provider) authCollection() (*core.Collection, error) {
	return p.app.FindCollectionByNameOrId(p.collectionID)
}

// --- authorize (GET shows the login page; customjs used /oauth/login) ---

func (p *Provider) handleAuthorizeGet(e *core.RequestEvent) error {
	p.cleanupExpired()
	q := e.Request.URL.Query()
	clientID, redirectURI := q.Get("client_id"), q.Get("redirect_uri")
	if clientID == "" || redirectURI == "" {
		return oauthError(e, http.StatusBadRequest, "invalid_request", "Missing client_id or redirect_uri.")
	}
	if q.Get("response_type") != "code" {
		return oauthError(e, http.StatusBadRequest, "unsupported_response_type", "Only response_type=code is supported.")
	}
	client, err := p.findClient(clientID)
	if err != nil || client.Revoked {
		return oauthError(e, http.StatusBadRequest, "invalid_client", "Unknown or revoked client_id.")
	}
	ok, legacy := matchesRedirect(client.RedirectURIs, redirectURI, client)
	if !ok {
		if legacy {
			return oauthError(e, http.StatusBadRequest, "invalid_request", MigrationMessage(client.RedirectURIs[0]))
		}
		return oauthError(e, http.StatusBadRequest, "invalid_request", "The redirect_uri is not registered for this client.")
	}
	challenge, method := q.Get("code_challenge"), q.Get("code_challenge_method")
	if challenge != "" || method != "" {
		if method != "S256" || !pkceRe.MatchString(challenge) {
			return oauthError(e, http.StatusBadRequest, "invalid_request", "Only code_challenge_method=S256 with a valid challenge is supported.")
		}
	} else if client.requiresPKCE() {
		return oauthError(e, http.StatusBadRequest, "invalid_request", "PKCE (code_challenge with S256) is required for this client.")
	}
	col, err := p.app.FindCollectionByNameOrId(p.cfg.TransactionsCollection())
	if err != nil {
		return apis.NewInternalServerError("Failed to access transactions", err)
	}
	txRaw := security.RandomString(32)
	csrfRaw := security.RandomString(32)
	rec := core.NewRecord(col)
	rec.Set("tx_hash", hashValue(txRaw))
	rec.Set("client_id", clientID)
	rec.Set("redirect_uri", redirectURI)
	rec.Set("state", q.Get("state"))
	rec.Set("scope", q.Get("scope"))
	rec.Set("code_challenge", challenge)
	rec.Set("code_challenge_method", method)
	rec.Set("auth_collection_id", p.collectionID)
	rec.Set("csrf_hash", hashValue(csrfRaw))
	rec.Set("expires_at", time.Now().Add(p.cfg.TransactionDuration))
	if err := p.app.Save(rec); err != nil {
		return apis.NewInternalServerError("Failed to save transaction", err)
	}
	secure := e.Request.TLS != nil || strings.HasPrefix(p.publicBase(e), "https://")
	http.SetCookie(e.Response, &http.Cookie{Name: txCookie, Value: txRaw, Path: p.cfg.LoginPath, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: int(p.cfg.TransactionDuration.Seconds())})
	name := client.Name
	if name == "" {
		name = clientID
	}
	return p.renderLogin(e, http.StatusOK, pageClient(client, redirectURI, loginPageData{ClientName: name, CSRF: csrfRaw}))
}

func (p *Provider) findTransaction(e *core.RequestEvent) (*core.Record, error) {
	cookie, err := e.Request.Cookie(txCookie)
	if err != nil || cookie.Value == "" {
		return nil, fmt.Errorf("missing transaction cookie")
	}
	rec, err := p.app.FindFirstRecordByData(p.cfg.TransactionsCollection(), "tx_hash", hashValue(cookie.Value))
	if err != nil || rec == nil {
		return nil, fmt.Errorf("unknown transaction")
	}
	if time.Now().After(rec.GetDateTime("expires_at").Time()) {
		_ = p.app.Delete(rec)
		return nil, fmt.Errorf("transaction expired")
	}
	if rec.GetString("auth_collection_id") != p.collectionID {
		return nil, fmt.Errorf("transaction is bound to a different auth collection")
	}
	return rec, nil
}

func (p *Provider) handleAuthorizePost(e *core.RequestEvent) error {
	tx, err := p.findTransaction(e)
	if err != nil {
		return oauthError(e, http.StatusBadRequest, "invalid_request", "Session expired. Please start the sign-in again.")
	}
	if err := e.Request.ParseForm(); err != nil {
		return oauthError(e, http.StatusBadRequest, "invalid_request", "Invalid form data.")
	}
	csrf := e.Request.FormValue("csrf_token")
	if csrf == "" || subtle.ConstantTimeCompare([]byte(hashValue(csrf)), []byte(tx.GetString("csrf_hash"))) != 1 {
		return oauthError(e, http.StatusForbidden, "invalid_request", "Invalid CSRF token.")
	}
	client, err := p.findClient(tx.GetString("client_id"))
	if err != nil || client.Revoked {
		return oauthError(e, http.StatusBadRequest, "invalid_client", "Unknown or revoked client.")
	}
	identity := strings.TrimSpace(e.Request.FormValue("identity"))
	if identity == "" {
		identity = strings.TrimSpace(e.Request.FormValue("username"))
	}
	password := e.Request.FormValue("password")
	name := client.Name
	if name == "" {
		name = client.ClientID
	}
	fail := func(msg string) error {
		return p.renderLogin(e, http.StatusBadRequest, pageClient(client, tx.GetString("redirect_uri"), loginPageData{ClientName: name, CSRF: csrf, Identity: identity, Error: msg}))
	}
	if identity == "" || password == "" {
		return fail("Enter your credentials.")
	}
	record, msg, err := p.authenticate(e, identity, password)
	if err != nil {
		return apis.NewInternalServerError("Authentication failed", err)
	}
	if msg != "" {
		return fail(msg)
	}
	if record == nil {
		return fail("Invalid credentials.")
	}
	// Issue a short-lived single-use code bound to everything validated above.
	rawCode := security.RandomString(40)
	codesCol, err := p.app.FindCollectionByNameOrId(p.cfg.CodesCollection())
	if err != nil {
		return apis.NewInternalServerError("Failed to access codes", err)
	}
	codeRec := core.NewRecord(codesCol)
	codeRec.Set("code_hash", hashValue(rawCode))
	codeRec.Set("client_id", client.ClientID)
	codeRec.Set("redirect_uri", tx.GetString("redirect_uri"))
	codeRec.Set("code_challenge", tx.GetString("code_challenge"))
	codeRec.Set("auth_collection_id", p.collectionID)
	codeRec.Set("user_id", record.Id)
	codeRec.Set("user_collection_id", record.Collection().Id)
	codeRec.Set("token_key_hash", hashValue(record.TokenKey()))
	codeRec.Set("scope", tx.GetString("scope"))
	codeRec.Set("expires_at", time.Now().Add(p.cfg.CodeDuration))
	codeRec.Set("consumed", false)
	if err := p.app.Save(codeRec); err != nil {
		return apis.NewInternalServerError("Failed to create authorization code", err)
	}
	_ = p.app.Delete(tx)
	http.SetCookie(e.Response, &http.Cookie{Name: txCookie, Value: "", Path: p.cfg.LoginPath, HttpOnly: true, MaxAge: -1})
	redirect := tx.GetString("redirect_uri")
	u, err := url.Parse(redirect)
	if err != nil {
		return apis.NewInternalServerError("Invalid redirect URI", err)
	}
	qs := u.Query()
	qs.Set("code", rawCode)
	if st := tx.GetString("state"); st != "" {
		qs.Set("state", st)
	}
	u.RawQuery = qs.Encode()
	return e.Redirect(http.StatusFound, u.String())
}

// authenticate performs local password auth against the configured
// collection. It returns (record, userMessage, err); a non-empty
// userMessage (including MFA refusal) means do not issue a code.
func (p *Provider) authenticate(e *core.RequestEvent, identity string, password string) (*core.Record, string, error) {
	collection, err := p.authCollection()
	if err != nil {
		return nil, "", err
	}
	if p.cfg.Upstream != nil {
		rec, err := p.cfg.Upstream.Authenticate(e.Request.Context(), p.app, collection, identity, password)
		if err != nil || rec == nil {
			return nil, "", err
		}
		if rec.Collection() == nil || rec.Collection().Id != p.collectionID {
			return nil, "This account cannot be used here.", nil
		}
		return p.checkEligible(e, collection, rec)
	}
	// Dispatch through PB's password endpoint so host auth hooks, rules,
	// rate limits and MFA remain authoritative. Never return its token to
	// the browser; the OAuth code exchange issues the final token.
	r, err := apis.NewRouter(p.app)
	if err != nil {
		return nil, "", err
	}
	mux, err := r.BuildMux()
	if err != nil {
		return nil, "", err
	}
	body, _ := json.Marshal(map[string]string{"identity": identity, "password": password})
	req, err := http.NewRequestWithContext(e.Request.Context(), http.MethodPost, "/api/collections/"+p.collectionID+"/auth-with-password", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = e.Request.RemoteAddr
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, req)
	var result struct {
		Token string `json:"token"`
		MFAID string `json:"mfaId"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &result)
	if result.MFAID != "" {
		return nil, "This account requires multi-factor authentication, which this sign-in flow cannot complete.", nil
	}
	if response.Code != http.StatusOK || result.Token == "" {
		return nil, "Invalid credentials or sign-in is not permitted.", nil
	}
	record, err := p.app.FindAuthRecordByToken(result.Token, core.TokenTypeAuth)
	if err != nil {
		return nil, "Invalid credentials.", nil
	}

	return p.checkEligible(e, collection, record)
}

func (p *Provider) checkEligible(e *core.RequestEvent, collection *core.Collection, record *core.Record) (*core.Record, string, error) {
	if record.Collection() == nil || record.Collection().Id != p.collectionID {
		return nil, "This account cannot be used here.", nil
	}
	if record.IsSuperuser() || record.Collection().Name == core.CollectionNameSuperusers {
		return nil, "This account cannot be used here.", nil
	}
	info := &core.RequestInfo{Method: http.MethodPost, Context: core.RequestInfoContextDefault, Auth: record}
	if collection.AuthRule != nil {
		if ok, err := p.app.CanAccessRecord(record, info, collection.AuthRule); err == nil && !ok {
			return nil, "This account is not allowed to sign in.", nil
		} else if err != nil {
			return nil, "", err
		}
	}
	if collection.MFA.Enabled {
		required := true
		if rule := strings.TrimSpace(collection.MFA.Rule); rule != "" {
			// MFA applies only to records satisfying the rule.
			ok, err := p.app.CanAccessRecord(record, info, &rule)
			if err != nil {
				return nil, "", err
			}
			required = ok
		}
		if required {
			return nil, "This account requires multi-factor authentication, which this sign-in flow cannot complete. Sign in through PocketBase directly or contact an administrator.", nil
		}
	}
	return record, "", nil
}

// --- token ---

func (p *Provider) handleToken(e *core.RequestEvent) error {
	e.Response.Header().Set("Cache-Control", "no-store")
	e.Response.Header().Set("Pragma", "no-cache")
	if err := e.Request.ParseForm(); err != nil {
		return oauthError(e, http.StatusBadRequest, "invalid_request", "Invalid request.")
	}
	form := e.Request.Form
	clientID := form.Get("client_id")
	clientSecret := form.Get("client_secret")
	if basicID, basicSecret, ok := e.Request.BasicAuth(); ok {
		if id, err := url.QueryUnescape(basicID); err == nil {
			basicID = id
		}
		if sec, err := url.QueryUnescape(basicSecret); err == nil {
			basicSecret = sec
		}
		if clientID == "" {
			clientID = basicID
		}
		if clientSecret == "" {
			clientSecret = basicSecret
		}
	}
	client, err := p.findClient(clientID)
	if err != nil || client.Revoked {
		return oauthError(e, http.StatusUnauthorized, "invalid_client", "Client authentication failed.")
	}
	if client.ClientType == "confidential" {
		stored := client.Record.GetString("secret_hash")
		if clientSecret == "" || stored == "" || !verifySecret(stored, clientSecret) {
			return oauthError(e, http.StatusUnauthorized, "invalid_client", "Client authentication failed.")
		}
	} else if clientSecret != "" {
		return oauthError(e, http.StatusUnauthorized, "invalid_client", "Public clients must not send a client secret.")
	}
	if form.Get("grant_type") != "authorization_code" {
		return oauthError(e, http.StatusBadRequest, "unsupported_grant_type", "Only grant_type=authorization_code is supported.")
	}
	rawCode := form.Get("code")
	redirectURI := form.Get("redirect_uri")
	if rawCode == "" || redirectURI == "" {
		return oauthError(e, http.StatusBadRequest, "invalid_request", "Missing code or redirect_uri.")
	}
	codeRec, err := p.app.FindFirstRecordByData(p.cfg.CodesCollection(), "code_hash", hashValue(rawCode))
	if err != nil || codeRec == nil {
		return oauthError(e, http.StatusBadRequest, "invalid_grant", "Invalid, expired or already used authorization code.")
	}
	invalid := func() error {
		return oauthError(e, http.StatusBadRequest, "invalid_grant", "Invalid, expired or already used authorization code.")
	}
	if codeRec.GetString("client_id") != client.ClientID || codeRec.GetString("auth_collection_id") != p.collectionID {
		return invalid()
	}
	if codeRec.GetString("redirect_uri") != redirectURI {
		return invalid()
	}
	if time.Now().After(codeRec.GetDateTime("expires_at").Time()) {
		_ = p.app.Delete(codeRec)
		return invalid()
	}
	// PKCE verification (before consuming, a bad verifier must not burn a
	// code that... actually consumption happens atomically below; verify
	// first so a wrong verifier is a clean invalid_grant).
	challenge := codeRec.GetString("code_challenge")
	verifier := form.Get("code_verifier")
	if challenge != "" {
		if !pkceRe.MatchString(verifier) {
			return invalid()
		}
		sum := sha256.Sum256([]byte(verifier))
		computed := base64.RawURLEncoding.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) != 1 {
			return invalid()
		}
	} else {
		if verifier != "" {
			return invalid() // PKCE downgrade prevention
		}
		if client.requiresPKCE() {
			return invalid() // PKCE was required but no challenge was bound
		}
	}
	// Atomically consume the code.
	consumedID := codeRec.Id
	var userID, userColID, scope string
	consumeErr := p.app.RunInTransaction(func(txApp core.App) error {
		fresh, err := txApp.FindRecordById(p.cfg.CodesCollection(), consumedID)
		if err != nil {
			return fmt.Errorf("gone")
		}
		if fresh.GetBool("consumed") {
			return fmt.Errorf("consumed")
		}
		fresh.Set("consumed", true)
		if err := txApp.Save(fresh); err != nil {
			return err
		}
		userID, userColID, scope = fresh.GetString("user_id"), fresh.GetString("user_collection_id"), fresh.GetString("scope")
		// Capture the token-key hash inside the transaction for revalidation.
		codeRec = fresh
		return nil
	})
	if consumeErr != nil {
		return invalid()
	}
	// Revalidate account eligibility and token-key state at exchange.
	collection, err := p.authCollection()
	if err != nil {
		return apis.NewInternalServerError("Auth collection unavailable", err)
	}
	record, err := p.app.FindRecordById(collection, userID)
	if err != nil || record == nil || record.Collection().Id != userColID || userColID != p.collectionID {
		return invalid()
	}
	if subtle.ConstantTimeCompare([]byte(hashValue(record.TokenKey())), []byte(codeRec.GetString("token_key_hash"))) != 1 {
		return invalid() // token key rotated between login and exchange
	}
	if _, msg, err := p.checkEligible(e, collection, record); err != nil {
		return apis.NewInternalServerError("Eligibility check failed", err)
	} else if msg != "" {
		return invalid()
	}
	var token string
	var expiresIn int64
	if p.cfg.TokenDuration > 0 {
		token, err = record.NewStaticAuthToken(p.cfg.TokenDuration)
		expiresIn = int64(p.cfg.TokenDuration.Seconds())
	} else {
		token, err = record.NewAuthToken()
		expiresIn = collection.AuthToken.Duration
	}
	if err != nil {
		return apis.NewInternalServerError("Failed to create auth token", err)
	}
	resp := map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": expiresIn}
	if scope != "" {
		resp["scope"] = scope
	}
	return e.JSON(http.StatusOK, resp)
}

// --- userinfo ---

func (p *Provider) handleUserinfo(e *core.RequestEvent) error {
	if e.Auth == nil {
		return oauthError(e, http.StatusUnauthorized, "invalid_token", "Authentication required.")
	}
	if e.Auth.Collection() == nil || e.Auth.Collection().Id != p.collectionID {
		return oauthError(e, http.StatusForbidden, "invalid_token", "Token was not issued for this auth collection.")
	}
	out := map[string]any{"sub": e.Auth.Id}
	if email := e.Auth.Email(); email != "" {
		out["email"] = email
		out["email_verified"] = e.Auth.Verified()
	}
	if e.Auth.Collection().Fields.GetByName("name") != nil {
		if v := e.Auth.GetString("name"); v != "" {
			out["name"] = v
		}
	}
	if e.Auth.Collection().Fields.GetByName("username") != nil {
		if v := e.Auth.GetString("username"); v != "" {
			out["preferred_username"] = v
		}
	}
	return e.JSON(http.StatusOK, out)
}
