package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func cookieValue(t *testing.T, setCookie string) string {
	t.Helper()
	parts := strings.SplitN(setCookie, ";", 2)
	if !strings.HasPrefix(parts[0], "dsession=") {
		t.Fatalf("expected dsession= prefix, got %q", parts[0])
	}
	return strings.TrimPrefix(parts[0], "dsession=")
}

func sign256(payload, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func signedCookie(email string, generation, expiry int64, key string) string {
	payload := fmt.Sprintf("2.%s.%d.%d", base64.RawURLEncoding.EncodeToString([]byte(email)), generation, expiry)
	return payload + "." + sign256(payload, key)
}

// testKeys is the one-key set most tests sign and verify with.
var testKeys = SingleSigningKey("signing-key")

func TestIssueAndVerifySessionCookie(t *testing.T) {
	setCookie := IssueSessionCookie("sami@example.com", 7, testKeys, true)
	for _, frag := range []string{"dsession=", "HttpOnly", "Path=/", "SameSite=Strict", "Max-Age=2592000", "Secure"} {
		if !strings.Contains(setCookie, frag) {
			t.Errorf("set-cookie %q missing fragment %q", setCookie, frag)
		}
	}
	session, ok := VerifySession(cookieValue(t, setCookie), testKeys)
	if !ok || session.Login != "sami@example.com" || session.Generation != 7 {
		t.Errorf("verify: got %#v valid=%t, want sami@example.com generation 7", session, ok)
	}
}

// An email holds the characters the payload's separators and a cookie value cannot: dots, a plus,
// an at sign. The cookie carries any of them back unchanged.
func TestSessionCookieRoundTripsAnyEmail(t *testing.T) {
	for _, email := range []string{"a.b+c@d.example", "first.last@sub.d.example", "o'neil=x;y@d.example"} {
		session, ok := VerifySession(cookieValue(t, IssueSessionCookie(email, 3, testKeys, false)), testKeys)
		if !ok || session.Login != email || session.Generation != 3 {
			t.Errorf("round trip of %q: got %#v valid=%t", email, session, ok)
		}
	}
}

// A cookie Dispatch issued before people were named by email (`<login>.<generation>.<expiry>.<sig>`)
// is signed with the same key, and still does not verify: no GitHub login comes back as a person.
func TestVerifySessionRefusesTheLoginCookieShape(t *testing.T) {
	payload := fmt.Sprintf("%s.%d.%d", "sjawhar", 0, time.Now().Add(time.Hour).UnixMilli())
	if session, ok := VerifySession(payload+"."+sign256(payload, "signing-key"), testKeys); ok {
		t.Errorf("login-shaped cookie verified as %#v", session)
	}
}

func TestIssueSessionCookieInsecureFlag(t *testing.T) {
	setCookie := IssueSessionCookie("sami@example.com", 0, testKeys, false)
	if strings.Contains(setCookie, "Secure") {
		t.Errorf("insecure mode should omit Secure: %q", setCookie)
	}
}

func TestVerifySessionRejectsTamperedCookie(t *testing.T) {
	value := cookieValue(t, IssueSessionCookie("sami@example.com", 0, testKeys, false))
	tampered := value[:len(value)-1] + "a"
	if value[len(value)-1] == 'a' {
		tampered = value[:len(value)-1] + "b"
	}
	if _, ok := VerifySession(tampered, testKeys); ok {
		t.Errorf("tampered cookie should not verify")
	}
	other := signedCookie("mallory@example.com", 0, time.Now().Add(time.Hour).UnixMilli(), "another-key")
	if _, ok := VerifySession(other, testKeys); ok {
		t.Errorf("cookie signed with another key should not verify")
	}
}

func TestVerifySessionRejectsExpiredCookie(t *testing.T) {
	fresh := signedCookie("sami@example.com", 0, time.Now().Add(time.Hour).UnixMilli(), "signing-key")
	if _, ok := VerifySession(fresh, testKeys); !ok {
		t.Fatalf("an unexpired cookie built like the issuer's does not verify: %q", fresh)
	}
	expired := signedCookie("sami@example.com", 0, time.Now().Add(-time.Hour).UnixMilli(), "signing-key")
	if _, ok := VerifySession(expired, testKeys); ok {
		t.Errorf("expired cookie should not verify")
	}
}

// parseKeys is ParseSigningKeys for a setting the test knows is valid.
func parseKeys(t *testing.T, setting string) SigningKeys {
	t.Helper()
	keys, err := ParseSigningKeys(setting)
	if err != nil {
		t.Fatalf("ParseSigningKeys(%q): %v", setting, err)
	}
	return keys
}

// DISPATCH_SIGNING_KEY is refused at boot for an empty entry or a repeated one, naming the entry
// by its position and never by its value, since the refusal reaches the boot log.
func TestParseSigningKeysRefusesAnEmptyOrRepeatedEntry(t *testing.T) {
	for _, test := range []struct {
		setting string
		want    string
	}{
		{setting: "", want: "entry 1 of 1 is empty"},
		{setting: "new-key  old-key", want: "entry 2 of 3 is empty"},
		{setting: "new-key\r\nold-key", want: "entry 2 of 3 is empty"},
		{setting: "new-key old-key new-key", want: "entry 3 of 3 repeats entry 1"},
		{setting: "new-key old-key old-key", want: "entry 3 of 3 repeats entry 2"},
	} {
		keys, err := ParseSigningKeys(test.setting)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("ParseSigningKeys(%q) = %v, %v; want a refusal naming %q", test.setting, keys.Fingerprints(), err, test.want)
			continue
		}
		for _, value := range []string{"new-key", "old-key"} {
			if strings.Contains(err.Error(), value) {
				t.Errorf("ParseSigningKeys(%q) refusal %q names the value %q", test.setting, err, value)
			}
		}
	}
}

// During a rotation one process holds "K1 K0" and another "K0 K1": a cookie signed with either key
// verifies under both, whichever key is current.
func TestACookieSignedWithEitherKeyVerifiesUnderBothOrders(t *testing.T) {
	expiry := time.Now().Add(time.Hour).UnixMilli()
	for _, setting := range []string{"K1 K0", "K0 K1"} {
		keys := parseKeys(t, setting)
		for _, key := range []string{"K0", "K1"} {
			session, ok := VerifySession(signedCookie("sami@example.com", 4, expiry, key), keys)
			if !ok || session.Login != "sami@example.com" || session.Generation != 4 {
				t.Errorf("a cookie signed with %s under %q: got %#v valid=%t, want sami@example.com generation 4", key, setting, session, ok)
			}
		}
	}
}

// A new cookie is signed with the first key only, so a process holding just that key verifies it
// and one holding just the second does not.
func TestANewCookieIsSignedWithTheFirstKey(t *testing.T) {
	value := cookieValue(t, IssueSessionCookie("sami@example.com", 0, parseKeys(t, "K1 K0"), false))
	if _, ok := VerifySession(value, SingleSigningKey("K1")); !ok {
		t.Errorf("a cookie issued under \"K1 K0\" does not verify under K1 alone")
	}
	if _, ok := VerifySession(value, SingleSigningKey("K0")); ok {
		t.Errorf("a cookie issued under \"K1 K0\" verifies under K0 alone, want it signed with K1")
	}
}

func TestACookieSignedWithAKeyOutsideTheSetIsRefused(t *testing.T) {
	cookie := signedCookie("mallory@example.com", 0, time.Now().Add(time.Hour).UnixMilli(), "K2")
	for _, setting := range []string{"K1 K0", "K0 K1", "K1"} {
		if session, ok := VerifySession(cookie, parseKeys(t, setting)); ok {
			t.Errorf("a cookie signed with K2 verified under %q as %#v", setting, session)
		}
	}
}

// A fingerprint is the first 16 bytes of the key's HMAC-SHA256 of the fixed label, in lowercase
// hex, one per key in the set's order, so processes holding the same keys in any order can tell.
func TestFingerprintsNameEachKeyCurrentFirst(t *testing.T) {
	fingerprint := func(key string) string {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte("dispatch signing key fingerprint v1"))
		return hex.EncodeToString(mac.Sum(nil)[:16])
	}
	got := parseKeys(t, "K1 K0").Fingerprints()
	if want := []string{fingerprint("K1"), fingerprint("K0")}; !slices.Equal(got, want) {
		t.Errorf("fingerprints of \"K1 K0\" = %q, want %q", got, want)
	}
	for _, value := range got {
		if len(value) != 32 || strings.ToLower(value) != value {
			t.Errorf("fingerprint %q, want 32 lowercase hex characters", value)
		}
	}
}

// A server with no DISPATCH_SIGNING_KEY keeps one key in its data dir: the first start writes it,
// a later one reads the same key back, and the set it yields holds that one key.
func TestLoadOrCreateSigningKeyYieldsTheFileKeyAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch", "signing-key")
	created, err := LoadOrCreateSigningKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the key file: %v", err)
	}
	if want := SingleSigningKey(string(stored)).Fingerprints(); !slices.Equal(created.Fingerprints(), want) {
		t.Errorf("created set %q, want the file's one key %q", created.Fingerprints(), want)
	}
	loaded, err := LoadOrCreateSigningKey(path)
	if err != nil || !slices.Equal(loaded.Fingerprints(), created.Fingerprints()) {
		t.Errorf("load again: %q, %v; want %q", loaded.Fingerprints(), err, created.Fingerprints())
	}
}

func TestNewSigningKeyIsFreshEachCall(t *testing.T) {
	first, err := NewSigningKey()
	if err != nil {
		t.Fatalf("first key: %v", err)
	}
	second, err := NewSigningKey()
	if err != nil {
		t.Fatalf("second key: %v", err)
	}
	if first == second {
		t.Fatalf("two calls returned the same key %q", first)
	}
	for _, key := range []string{first, second} {
		decoded, err := base64.RawURLEncoding.DecodeString(key)
		if err != nil || len(decoded) != 32 {
			t.Errorf("key %q decodes to %d bytes (err %v), want 32 bytes of base64url", key, len(decoded), err)
		}
	}
}
