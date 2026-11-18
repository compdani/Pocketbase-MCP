package codex

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func signJWT(t *testing.T, key *rsa.PrivateKey, header map[string]any, claims map[string]any) string {
	t.Helper()
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func claimsFor(issuer string) map[string]any {
	return map[string]any{
		"iss":   issuer,
		"sub":   "user-1",
		"email": "user@example.com",
		"aud":   []string{"client-1"},
		"exp":   time.Now().Add(time.Hour).Unix(),
		"nbf":   time.Now().Add(-time.Minute).Unix(),
		"nonce": "nonce-1",
	}
}

func TestParseAndVerifyJWT(t *testing.T) {
	key := testKey(t)
	raw := signJWT(t, key, map[string]any{"alg": "RS256", "kid": "k1"}, claimsFor("https://issuer.example"))

	claims, err := parseAndVerifyJWT(raw, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "user-1" || claims.Email != "user@example.com" || claims.Nonce != "nonce-1" {
		t.Fatalf("claims = %+v", claims)
	}
	if err := validateClaims(claims, "https://issuer.example", "client-1", "nonce-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := validateClaims(claims, "https://wrong.example", "client-1", "nonce-1", time.Now()); err == nil {
		t.Error("wrong issuer accepted")
	}
	if err := validateClaims(claims, "https://issuer.example", "client-2", "nonce-1", time.Now()); err == nil {
		t.Error("wrong audience accepted")
	}
	if err := validateClaims(claims, "https://issuer.example", "client-1", "other", time.Now()); err == nil {
		t.Error("wrong nonce accepted")
	}
	expired := claimsFor("https://issuer.example")
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	rawExpired := signJWT(t, key, map[string]any{"alg": "RS256"}, expired)
	claimsExpired, err := parseAndVerifyJWT(rawExpired, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateClaims(claimsExpired, "https://issuer.example", "client-1", "nonce-1", time.Now()); err == nil {
		t.Error("expired token accepted")
	}
}

func TestParseAndVerifyJWTRejectsBadInput(t *testing.T) {
	key := testKey(t)
	raw := signJWT(t, key, map[string]any{"alg": "RS256"}, claimsFor("https://issuer.example"))
	tampered := raw[:len(raw)-2] + "AA"
	if _, err := parseAndVerifyJWT(tampered, &key.PublicKey); err == nil {
		t.Error("tampered signature accepted")
	}
	hs := signJWT(t, key, map[string]any{"alg": "HS256"}, claimsFor("https://issuer.example"))
	if _, err := parseAndVerifyJWT(hs, &key.PublicKey); err == nil {
		t.Error("HS256 accepted")
	}
	if _, err := parseAndVerifyJWT("not-a-jwt", &key.PublicKey); err == nil {
		t.Error("malformed token accepted")
	}
	if _, err := parseAndVerifyJWT(raw, nil); err == nil {
		t.Error("nil key accepted")
	}
}
