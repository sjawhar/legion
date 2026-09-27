package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// DeploymentInstructionsFile is the copy of the operator's instructions every pane's prompt reads,
// under the state directory.
const DeploymentInstructionsFile = "deployment-instructions.md"

// ReadSecretPointer is the trimmed contents of the file a pointer key names — `variable` is the
// key as the operator wrote it (`envoy_token_file`, `operator_token_file`), so the refusal reads
// in their words. A set pointer is authoritative: a missing, unreadable, or blank file is a
// refusal naming the key and the path, never a fallback to another source
// (packages/daemon/src/daemon/secrets.ts:10-22). The contents never appear in an error.
func ReadSecretPointer(variable, file string) (string, error) {
	contents, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s names %s, which could not be read: %w", variable, file, err)
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" {
		return "", fmt.Errorf("%s names %s, which is empty", variable, file)
	}
	return secret, nil
}

// ReadPrivateSecretPointer is ReadSecretPointer for a secret a group- or other-readable copy of
// would be a second way in — the operator bearer, which buys a controller capability and opens every
// operator route, and the NATS nkey seed file `legion controller start` hands its Oh My Pi —
// `variable` is the key or flag as the operator wrote it (`operator_token_file`,
// `--operator-token-file`, `nats_nkey_seed_file`). The file is opened once, and the open descriptor
// must be a regular file whose mode grants its group and others nothing before its trimmed,
// non-empty contents are read from that same descriptor, so nothing swapped in between a check and
// a read is ever read (packages/daemon/src/cli/controller-start.ts:177-199, which stats and reads
// separately). The open does not block, so a FIFO is refused rather than waited on. The contents
// never appear in an error.
func ReadPrivateSecretPointer(variable, file string) (string, error) {
	return readSecretFile(variable, file, func(os.FileInfo) secretMode { return ownerOnly })
}

// ReadGroupSecretPointer is ReadPrivateSecretPointer for the NATS nkey seed the daemon reads, whose
// group may read it when another uid owns it: a daemon running as a non-root uid in a pod reads a
// kubelet-mounted Secret file, root's, only through the pod's fsGroup. Such a file is refused only
// when its group can write it or others can touch it at all. A file the reading uid owns — a seed
// on a shared host — is held to ReadPrivateSecretPointer's 0600.
func ReadGroupSecretPointer(variable, file string) (string, error) {
	return readSecretFile(variable, file, func(info os.FileInfo) secretMode {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != readerUID() {
			return groupMount
		}
		return ownerOnly
	})
}

// readerUID is the uid whose own files ReadGroupSecretPointer holds to 0600; tests replace it.
var readerUID = os.Geteuid

// secretMode is the mode a secret file must keep: forbidden holds the permission bits refused, and
// says and fix word the refusal.
type secretMode struct {
	forbidden os.FileMode
	says, fix string
}

var (
	ownerOnly  = secretMode{0o077, "is readable by its group or others", "chmod 0600 it"}
	groupMount = secretMode{0o027, "is writable by its group or open to others", "chmod g-w,o-rwx it, or mount it with defaultMode: 0440"}
)

// readSecretFile is ReadPrivateSecretPointer's open, check, and read, refusing the bits of the mode
// modeFor answers for the open file.
func readSecretFile(variable, file string, modeFor func(os.FileInfo) secretMode) (string, error) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("%s names %s, which could not be read: %w", variable, file, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%s names %s, which could not be read: %w", variable, file, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s names %s, which is not a regular file", variable, file)
	}
	if m, mode := modeFor(info), info.Mode().Perm(); mode&m.forbidden != 0 {
		return "", fmt.Errorf("%s %s %s (mode %#o); %s", variable, file, m.says, mode, m.fix)
	}
	contents, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("%s names %s, which could not be read: %w", variable, file, err)
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", fmt.Errorf("%s names %s, which is empty", variable, file)
	}
	return token, nil
}

// ReadDeploymentInstructions is the operator's instructions file, refused naming the operator's
// path when it is missing, unreadable, or blank: a configured file with nothing in it is a
// misconfiguration, not an empty fragment. It writes nothing, so `legion controller start` runs it
// before the one daemon call it makes (packages/daemon/src/daemon/deployment-instructions.ts).
func ReadDeploymentInstructions(instructionsPath string) ([]byte, error) {
	contents, err := os.ReadFile(instructionsPath)
	if err != nil {
		return nil, fmt.Errorf("instructions file %s could not be read: %w", instructionsPath, err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		return nil, fmt.Errorf("instructions file %s is empty", instructionsPath)
	}
	return contents, nil
}

// MaterializeDeploymentInstructions reads the operator's instructions file and writes
// `# Deployment instructions (<legionID>)`, a blank line, and its contents verbatim to
// `<stateDir>/deployment-instructions.md`, returning that path — the file every pane's one
// `--append-system-prompt` word ends with, `$(cat <this file>)`, so a pane gets exactly what boot
// read and never the operator's own path, which may change underneath a running daemon
// (packages/daemon/src/daemon/deployment-instructions.ts:29-46). legionID is `project` as the
// operator wrote it.
//
// A file ReadDeploymentInstructions refuses writes nothing. Boot calls this once, before the first
// pane opens.
func MaterializeDeploymentInstructions(instructionsPath, stateDir, legionID string) (string, error) {
	contents, err := ReadDeploymentInstructions(instructionsPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("create state directory %s: %w", stateDir, err)
	}
	target := filepath.Join(stateDir, DeploymentInstructionsFile)
	body := fmt.Sprintf("# Deployment instructions (%s)\n\n%s", legionID, contents)
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", target, err)
	}
	return target, nil
}
