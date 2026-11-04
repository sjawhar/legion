package secrets_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

const azureLoginValue = `{"tenant_id":"tenant-id","client_id":"client-id"}`

func azureProjectedToken(payload string) string {
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".c2lnbmF0dXJl"
}

func TestAzureLoginMintReleasesTheCurrentProjectedToken(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	minter := secrets.AzureLogin{
		TokenFile: file,
		Now:       func() time.Time { return time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC) },
	}
	for _, row := range []struct{ claims, expiry string }{
		{`{"exp":1793754000}`, "2026-11-04T01:00:00Z"},
		{`{"exp":1793757600}`, "2026-11-04T02:00:00Z"},
	} {
		token := azureProjectedToken(row.claims)
		if err := os.WriteFile(file, []byte("\n "+token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		minted, err := minter.Mint(t.Context(), secrets.MintRequest{Value: azureLoginValue})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(minted), &got); err != nil {
			t.Fatalf("Mint returned invalid JSON: %v", err)
		}
		if len(got) != 4 || got["tenant_id"] != "tenant-id" || got["client_id"] != "client-id" || got["federated_token"] != token || got["expires_at"] != row.expiry {
			t.Fatal("Mint did not return the stored identity, the current projected token and its JWT expiry")
		}
	}
}

func TestAzureLoginMintRefusesMalformedStoredIdentities(t *testing.T) {
	for _, value := range []string{
		"stored-credential-must-not-leak", "null", "[]", `{`, `{}`, `{"tenant_id":"tenant-id"}`,
		`{"client_id":"client-id"}`, `{"tenant_id":" ","client_id":"client-id"}`,
		`{"tenant_id":"tenant-id","client_id":" "}`, `{"tenant_id":123,"client_id":"client-id"}`,
	} {
		got, err := (secrets.AzureLogin{}).Mint(t.Context(), secrets.MintRequest{Value: value})
		if got != "" || secrets.MintErrorClass(err) != "stored-value" {
			t.Fatalf("Mint of malformed identity returned class %s, value present %t; want stored-value and no value", secrets.MintErrorClass(err), got != "")
		}
		if strings.Contains(err.Error(), "stored-credential-must-not-leak") {
			t.Fatal("Mint error disclosed the stored value")
		}
	}
}

func TestAzureLoginMintRefusesUnreadableProjectedTokens(t *testing.T) {
	for _, file := range []string{filepath.Join(t.TempDir(), "do-not-print-this-token"), t.TempDir()} {
		got, err := (secrets.AzureLogin{TokenFile: file}).Mint(t.Context(), secrets.MintRequest{Value: azureLoginValue})
		if got != "" || secrets.MintErrorClass(err) != "upstream" {
			t.Fatalf("Mint of unreadable file returned class %s, value present %t; want upstream and no value", secrets.MintErrorClass(err), got != "")
		}
		if strings.Contains(err.Error(), "do-not-print-this-token") {
			t.Fatal("Mint error disclosed the token file's path")
		}
	}
}

func TestAzureLoginMintRefusesInvalidOrExpiredProjectedTokensWithoutDisclosingThem(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	for name, token := range map[string]string{
		"empty":             "",
		"whitespace":        " \n ",
		"not a JWT":         "do-not-print-this-token",
		"invalid base64":    "header.@@do-not-print-this-token.signature",
		"invalid JSON":      azureProjectedToken(`{"exp":"do-not-print-this-token"`),
		"no signature":      "header.eyJleHAiOjE3OTM3NTQwMDB9.",
		"extra segment":     "header.eyJleHAiOjE3OTM3NTQwMDB9.signature.extra",
		"no expiry":         azureProjectedToken(`{"sub":"do-not-print-this-token"}`),
		"null expiry":       azureProjectedToken(`{"exp":null}`),
		"string expiry":     azureProjectedToken(`{"exp":"1793754000"}`),
		"fractional expiry": azureProjectedToken(`{"exp":1793754000.5}`),
		"past expiry":       azureProjectedToken(`{"exp":1793750399}`),
		"at expiry":         azureProjectedToken(`{"exp":1793750400}`),
		"negative expiry":   azureProjectedToken(`{"exp":-1}`),
		"unencodable date":  azureProjectedToken(`{"exp":9223372036854775807}`),
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := (secrets.AzureLogin{TokenFile: file, Now: func() time.Time { return now }}).Mint(t.Context(), secrets.MintRequest{Value: azureLoginValue})
			if got != "" || secrets.MintErrorClass(err) != "upstream" {
				t.Fatalf("Mint returned class %s, value present %t; want upstream and no value", secrets.MintErrorClass(err), got != "")
			}
			if strings.Contains(err.Error(), "do-not-print-this-token") || (strings.TrimSpace(token) != "" && strings.Contains(err.Error(), token)) {
				t.Fatal("Mint error disclosed the projected token")
			}
		})
	}
}
