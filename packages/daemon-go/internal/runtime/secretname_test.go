package runtime

import "testing"

// A trailing credential segment, optionally followed by _FILE, or PRIVATE_KEY anywhere, in any
// case — and nothing that merely contains one of those words.
func TestIsSecretLikeName(t *testing.T) {
	for name, want := range map[string]bool{
		"ANTHROPIC_API_KEY":      true,
		"GH_TOKEN":               true,
		"LEGION_GRANT":           true,
		"db_password":            true,
		"CLIENT_SECRET_FILE":     true,
		"GITHUB_PAT":             true,
		"AWS_CREDENTIALS":        true,
		"MY_PRIVATE_KEY_PATH":    true,
		"X_PASSWD":               true,
		"TOKENIZER":              false,
		"X_PATH":                 false,
		"X_KEYBOARD":             false,
		"PATH":                   false,
		"HOME":                   false,
		"XDG_CONFIG_HOME":        false,
		"LEGION_BOOT_TOKEN_FILE": true,
	} {
		if got := IsSecretLikeName(name); got != want {
			t.Errorf("IsSecretLikeName(%q) = %v, want %v", name, got, want)
		}
	}
}
