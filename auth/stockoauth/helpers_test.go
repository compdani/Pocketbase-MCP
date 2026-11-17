package stockoauth

import (
	"strings"
	"testing"
)

func TestSecretHashRoundTrip(t *testing.T) {
	h := hashSecret("s3cr3t-value")
	if strings.Contains(h, "s3cr3t") {
		t.Fatal("hash must not contain the secret")
	}
	if !verifySecret(h, "s3cr3t-value") || verifySecret(h, "wrong") {
		t.Fatal("verify failed")
	}
}

func TestRedirectMatching(t *testing.T) {
	conf := &clientRecord{ClientType: "confidential"}
	pub := &clientRecord{ClientType: "public"}
	reg := []string{"https://chatgpt.com/connector/oauth/callback"}
	if ok, _ := matchesRedirect(reg, "https://chatgpt.com/connector/oauth/callback", conf); !ok {
		t.Fatal("exact match failed")
	}
	if ok, _ := matchesRedirect(reg, "https://chatgpt.com/other", conf); ok {
		t.Fatal("domain-only style match must fail")
	}
	loop := []string{"http://127.0.0.1:8080/callback"}
	if ok, _ := matchesRedirect(loop, "http://127.0.0.1:9999/callback", pub); !ok {
		t.Fatal("loopback port flexibility for public clients failed")
	}
	if ok, _ := matchesRedirect(loop, "http://127.0.0.1:9999/callback", conf); ok {
		t.Fatal("loopback flexibility must not apply to confidential clients")
	}
	if !IsLegacyDomainOnlyRedirect("https://example.com") || IsLegacyDomainOnlyRedirect("https://example.com/callback") {
		t.Fatal("legacy detection wrong")
	}
	if msg := MigrationMessage("https://example.com"); !strings.Contains(msg, "full callback URL") {
		t.Fatalf("migration message: %s", msg)
	}
}

func TestDCRRedirectValidation(t *testing.T) {
	if err := ValidateRedirectURIsForRegistration([]string{"http://evil.example.com/cb"}, true); err == nil {
		t.Fatal("non-https non-loopback must be rejected")
	}
	if err := ValidateRedirectURIsForRegistration([]string{"http://127.0.0.1:1234/cb"}, true); err != nil {
		t.Fatalf("loopback http should pass: %v", err)
	}
}
