package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// The operator bearer buys a controller capability and opens every operator route, so the file
// holding it is held to what the open file is: a regular file only its owner can read, with a
// token in it. A group- or other-readable copy is refused naming the path and the mode, and a FIFO
// is refused rather than waited on (packages/daemon/src/cli/controller-start.ts:177-199).
func TestReadOperatorTokenFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, contents string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	good := write("owner-only", "  op-tok\n", 0o600)
	token, err := ReadOperatorTokenFile("--operator-token-file", good)
	if err != nil || token != "op-tok" {
		t.Fatalf("ReadOperatorTokenFile(0600) = %q, %v; want the trimmed token", token, err)
	}

	for _, tc := range []struct {
		name, path, want string
	}{
		{"group-readable", write("group", "op-tok\n", 0o640), "%s is readable by its group or others (mode 0640); chmod 0600 it"},
		{"other-readable", write("other", "op-tok\n", 0o604), "%s is readable by its group or others (mode 0604); chmod 0600 it"},
		{"blank", write("blank", " \n\t", 0o600), "names %s, which is empty"},
		{"absent", filepath.Join(dir, "absent"), "names %s, which could not be read: "},
		{"a directory", dir, "names %s, which is not a regular file"},
		{"a FIFO", fifo, "names %s, which is not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := ReadOperatorTokenFile("operator_token_file", tc.path)
				done <- err
			}()
			select {
			case err := <-done:
				want := "operator_token_file " + strings.Replace(tc.want, "%s", tc.path, 1)
				if err == nil || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("err = %v, want it to start %q", err, want)
				}
				if strings.Contains(err.Error(), "op-tok") {
					t.Fatalf("the refusal carries the token: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ReadOperatorTokenFile is still waiting on the file")
			}
		})
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
