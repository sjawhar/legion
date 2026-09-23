package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A set pointer is authoritative: its file's trimmed contents, or a refusal naming the pointer
// and the path — never a fallback (packages/daemon/src/daemon/secrets.ts:10-22).
func TestReadSecretPointer(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "ENVOY_TOKEN")
	if err := os.WriteFile(good, []byte("  tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blank := filepath.Join(dir, "BLANK")
	if err := os.WriteFile(blank, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "ABSENT")

	secret, err := ReadSecretPointer("envoy_token_file", good)
	if err != nil {
		t.Fatalf("ReadSecretPointer: %v", err)
	}
	if secret != "tok-123" {
		t.Errorf("secret = %q, want the trimmed contents", secret)
	}

	if _, err := ReadSecretPointer("envoy_token_file", blank); err == nil ||
		err.Error() != "envoy_token_file names "+blank+", which is empty" {
		t.Errorf("blank file: err = %v", err)
	}

	_, err = ReadSecretPointer("operator_token_file", absent)
	if err == nil || !strings.HasPrefix(err.Error(), "operator_token_file names "+absent+", which could not be read: ") {
		t.Errorf("absent file: err = %v", err)
	}
}

// The copy every pane's prompt `$(cat)`s: a heading naming the legion as the operator wrote it,
// a blank line, and the operator's file verbatim (deployment-instructions.ts:37-46).
func TestMaterializeDeploymentInstructionsWritesTheHeadedCopy(t *testing.T) {
	source := filepath.Join(t.TempDir(), "instructions.md")
	content := "Run `make check` before every push.\n\n- never force-push\n"
	if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")

	written, err := MaterializeDeploymentInstructions(source, stateDir, "sjawhar/legion")
	if err != nil {
		t.Fatalf("MaterializeDeploymentInstructions: %v", err)
	}
	if want := filepath.Join(stateDir, "deployment-instructions.md"); written != want {
		t.Errorf("path = %q, want %q", written, want)
	}
	got, err := os.ReadFile(written)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# Deployment instructions (sjawhar/legion)\n\n" + content; string(got) != want {
		t.Errorf("contents = %q, want %q", got, want)
	}
}

// A configured key with nothing to append is a misconfiguration, never an empty fragment
// (deployment-instructions.ts:6-27): the refusal names the operator's path, and nothing is written.
func TestMaterializeDeploymentInstructionsRefusesAnUnusableFile(t *testing.T) {
	dir := t.TempDir()
	blank := filepath.Join(dir, "blank.md")
	if err := os.WriteFile(blank, []byte("\n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "absent.md")

	for _, tc := range []struct {
		name, path, want string
	}{
		{name: "blank", path: blank, want: "instructions file " + blank + " is empty"},
		{name: "absent", path: absent, want: "instructions file " + absent + " could not be read: "},
		{name: "a directory", path: dir, want: "instructions file " + dir + " could not be read: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			_, err := MaterializeDeploymentInstructions(tc.path, stateDir, "demo")
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to start %q", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(stateDir, "deployment-instructions.md")); !os.IsNotExist(statErr) {
				t.Errorf("a refused file still wrote the copy (stat: %v)", statErr)
			}
		})
	}
}
