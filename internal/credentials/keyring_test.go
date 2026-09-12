package credentials

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestDarwinStoreCommandKeepsRawSecretOutOfCommandAndRoundTrips(t *testing.T) {
	secret := `{"access_token":"marker + spaces / newline\\n"}`
	command, err := darwinStoreCommand("github-account", secret)
	if err != nil {
		t.Fatalf("darwinStoreCommand: %v", err)
	}
	if strings.Contains(command, secret) {
		t.Fatal("interactive Keychain command contains the raw secret")
	}
	prefix := " -w " + darwinEncodedSecretPrefix
	start := strings.Index(command, prefix)
	if start < 0 || !strings.HasSuffix(command, "\n") {
		t.Fatalf("unexpected interactive Keychain command %q", command)
	}
	encoded := strings.TrimSuffix(command[start+len(" -w "):], "\n")
	decoded, err := decodeDarwinSecret(encoded)
	if err != nil {
		t.Fatalf("decodeDarwinSecret: %v", err)
	}
	if decoded != secret {
		t.Fatalf("decoded secret = %q, want %q", decoded, secret)
	}
}

func TestDarwinStoreCommandRejectsUnsafeAccountAndOversizedInput(t *testing.T) {
	if validAccount("account\n-w injected") {
		t.Fatal("account with command separators was accepted")
	}
	secret := strings.Repeat("x", maximumSecurityInputLine)
	if _, err := darwinStoreCommand("account", secret); err == nil {
		t.Fatal("oversized Keychain command was accepted")
	}
}

func TestDecodeDarwinSecretSupportsLegacyValuesAndRejectsBadEncoding(t *testing.T) {
	const legacy = `{"access_token":"legacy"}`
	if got, err := decodeDarwinSecret(legacy); err != nil || got != legacy {
		t.Fatalf("legacy value = %q, %v", got, err)
	}
	encoded := darwinEncodedSecretPrefix + base64.RawStdEncoding.EncodeToString([]byte("current"))
	if got, err := decodeDarwinSecret(encoded); err != nil || got != "current" {
		t.Fatalf("encoded value = %q, %v", got, err)
	}
	if _, err := decodeDarwinSecret(darwinEncodedSecretPrefix + "%%%"); err == nil {
		t.Fatal("invalid encoded value was accepted")
	}
}
