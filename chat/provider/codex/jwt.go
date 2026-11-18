package codex

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The ID token verifier is deliberately small and standard-library only. The
// module has a no-third-party-libraries rule, and the token shape used by the
// ChatGPT authorization server is a plain RS256 JWT.

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type idClaims struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Email     string          `json:"email"`
	Nonce     string          `json:"nonce"`
	Expiry    json.RawMessage `json:"exp"`
	NotBefore json.RawMessage `json:"nbf"`
	Audience  json.RawMessage `json:"aud"`
}

// parseAndVerifyJWT verifies the signature and returns the claims. The caller
// supplies the key for the token's kid.
func parseAndVerifyJWT(raw string, key *rsa.PublicKey) (*idClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	headerRaw, err := decodeJWTPart(parts[0])
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported signing algorithm %q", header.Alg)
	}
	if key == nil {
		return nil, errors.New("no signing key")
	}
	sig, err := decodeJWTPart(parts[2])
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("signature mismatch")
	}
	claimsRaw, err := decodeJWTPart(parts[1])
	if err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	var claims idClaims
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	return &claims, nil
}

func decodeJWTPart(part string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err == nil {
		return raw, nil
	}
	// Some issuers pad the segments; the padded URL alphabet is the same bytes.
	return base64.URLEncoding.DecodeString(part)
}

// validateClaims checks issuer, audience, expiry, nonce and subject against the
// authorization attempt.
func validateClaims(claims *idClaims, issuer, clientID, nonce string, now time.Time) error {
	if claims == nil {
		return errors.New("no claims")
	}
	if claims.Issuer != issuer {
		return fmt.Errorf("issuer %q is not %q", claims.Issuer, issuer)
	}
	audience, err := claimStrings(claims.Audience)
	if err != nil {
		return fmt.Errorf("audience: %w", err)
	}
	if !containsString(audience, clientID) {
		return errors.New("audience does not include the client id")
	}
	if claims.Subject == "" {
		return errors.New("no subject")
	}
	exp, err := claimTime(claims.Expiry)
	if err != nil || exp.IsZero() {
		return errors.New("no expiry")
	}
	const skew = 30 * time.Second
	if !exp.After(now.Add(-skew)) {
		return errors.New("token has expired")
	}
	if nbf, err := claimTime(claims.NotBefore); err == nil && !nbf.IsZero() && nbf.After(now.Add(skew)) {
		return errors.New("token is not valid yet")
	}
	if nonce != "" && claims.Nonce != nonce {
		return errors.New("nonce mismatch")
	}
	return nil
}

func claimString(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func claimStrings(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, err
	}
	return []string{single}, nil
}

func claimTime(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, nil
	}
	var unix float64
	if err := json.Unmarshal(raw, &unix); err != nil {
		// Some tokens encode numeric dates as strings.
		var text string
		if err2 := json.Unmarshal(raw, &text); err2 != nil {
			return time.Time{}, err
		}
		if _, err2 := fmt.Sscanf(text, "%f", &unix); err2 != nil {
			return time.Time{}, err2
		}
	}
	return time.Unix(int64(unix), 0).UTC(), nil
}

func containsString(list []string, want string) bool {
	for _, value := range list {
		if value == want {
			return true
		}
	}
	return false
}
