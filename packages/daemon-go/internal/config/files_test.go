package config

import (
	"fmt"
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
func TestReadPrivateSecretPointer(t *testing.T) {
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
	token, err := ReadPrivateSecretPointer("--operator-token-file", good)
	if err != nil || token != "op-tok" {
		t.Fatalf("ReadPrivateSecretPointer(0600) = %q, %v; want the trimmed token", token, err)
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
				_, err := ReadPrivateSecretPointer("operator_token_file", tc.path)
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
				t.Fatal("ReadPrivateSecretPointer is still waiting on the file")
			}
		})
	}
}

// ReadGroupSecretPointer holds a file to ReadPrivateSecretPointer's rule unless root owns it and the
// reader is not root: a kubelet-mounted Secret, root's, read through the pod's fsGroup, may also be
// read by its group, and one its group can write or others can touch at all is refused naming the
// path, the mode and both remedies. A file the reader owns, or another non-root uid owns, keeps
// 0600. A FIFO is refused either way rather than waited on.
func TestReadGroupSecretPointer(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(" seed\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o640); err != nil {
		t.Fatal(err)
	}
	refused := func(t *testing.T, path, want string) {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			_, err := ReadGroupSecretPointer("nats_nkey_seed_file", path)
			done <- err
		}()
		select {
		case err := <-done:
			if want := strings.Replace(want, "%s", path, 1); err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ReadGroupSecretPointer is still waiting on the file")
		}
	}
	groupRefusal := func(mode string) string {
		return "nats_nkey_seed_file %s is writable by its group or open to others (mode " + mode + "); chmod g-w,o-rwx it, or mount it with defaultMode: 0440"
	}
	ownerRefusal := func(mode string) string {
		return "nats_nkey_seed_file %s is readable by its group or others (mode " + mode + "); chmod 0600 it"
	}

	t.Run("a file the reader owns", func(t *testing.T) {
		if got, err := ReadGroupSecretPointer("nats_nkey_seed_file", write("own-0600", 0o600)); err != nil || got != "seed" {
			t.Errorf("ReadGroupSecretPointer(0600) = %q, %v; want the trimmed secret", got, err)
		}
		refused(t, write("own-0440", 0o440), ownerRefusal("0440"))
		refused(t, write("own-0640", 0o640), ownerRefusal("0640"))
		refused(t, write("own-0604", 0o604), ownerRefusal("0604"))
		refused(t, fifo, "nats_nkey_seed_file names %s, which is not a regular file")
	})
	// The test's files are its own; the seams make them read as another uid's.
	asReader := func(t *testing.T, uid int) {
		self := readerUID
		readerUID = func() int { return uid }
		t.Cleanup(func() { readerUID = self })
	}
	t.Run("a root-owned file", func(t *testing.T) {
		asReader(t, os.Geteuid()+1)
		root := mountOwner
		mountOwner = uint32(os.Geteuid())
		t.Cleanup(func() { mountOwner = root })
		for _, mode := range []os.FileMode{0o600, 0o440, 0o640} {
			if got, err := ReadGroupSecretPointer("nats_nkey_seed_file", write("root-"+mode.String(), mode)); err != nil || got != "seed" {
				t.Errorf("ReadGroupSecretPointer(%#o) = %q, %v; want the trimmed secret", mode, got, err)
			}
		}
		for _, mode := range []os.FileMode{0o604, 0o602, 0o660, 0o620, 0o644} {
			octal := fmt.Sprintf("%#o", mode)
			refused(t, write("refused-"+octal, mode), groupRefusal(octal))
		}
		refused(t, fifo, "nats_nkey_seed_file names %s, which is not a regular file")
	})
	t.Run("a file another non-root uid owns", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("the test's own files are root's")
		}
		asReader(t, os.Geteuid()+1)
		if got, err := ReadGroupSecretPointer("nats_nkey_seed_file", write("alice-0600", 0o600)); err != nil || got != "seed" {
			t.Errorf("ReadGroupSecretPointer(0600) = %q, %v; want the trimmed secret", got, err)
		}
		refused(t, write("alice-0640", 0o640), ownerRefusal("0640"))
		refused(t, write("alice-0440", 0o440), ownerRefusal("0440"))
	})
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
