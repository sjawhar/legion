package shim

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The readers below are the command's refusals, each one naming the flag and the path or value at
// fault, and each run before the shim dials or spawns anything (worker-shim.ts:630-676). A pane
// has nothing to show but that one line.

// ParseAddress reads --connect: the daemon's worker stream as `unix:///<absolute path>` or
// `tcp://<host>:<port>`, the two families the listener accepts. It returns the network and
// address net.Dial takes. The TCP rules are the shipped ones (worker-shim.ts:544-576): a host, a
// port in 1..65535, and nothing else — no path beyond "/", no query, fragment, or user.
func ParseAddress(value string) (network, address string, err error) {
	malformed := fmt.Errorf("--connect must be unix:///<path> or tcp://<host>:<port>, got %q", value)
	u, err := url.Parse(value)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", "", malformed
	}
	switch {
	case u.Scheme == "unix" && strings.HasPrefix(value, "unix://"):
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.HasSuffix(u.Path, "/") {
			return "", "", malformed
		}
		return "unix", u.Path, nil
	case u.Scheme == "tcp" && strings.HasPrefix(value, "tcp://"):
		host, port := u.Hostname(), u.Port()
		n, err := strconv.Atoi(port)
		if host == "" || err != nil || n < 1 || n > 65535 || (u.Path != "" && u.Path != "/") {
			return "", "", malformed
		}
		return "tcp", net.JoinHostPort(host, port), nil
	default:
		return "", "", malformed
	}
}

// ReadBootToken reads --boot-token-file: the 0600 file the runtime wrote the pane's boot token
// to. The token is the file's trimmed contents, and a file with none is refused.
func ReadBootToken(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("--boot-token-file %s is unreadable: %w", path, err)
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", fmt.Errorf("--boot-token-file %s is blank", path)
	}
	return token, nil
}

// ReadProviderEnv reads --provider-env-dir: NAME=trimmed contents for every regular file in dir,
// for the wrapped process's environment only (worker-shim.ts:578-620). Symlinks are followed —
// a Kubernetes Secret mount is KEY -> ..data/KEY beside directories, which are skipped.
//
// Two names are not exported. A NAME whose NAME_FILE pointer the shim's own environment carries
// is read from that file by its consumer, and must not also sit in the environment every tool
// the agent runs inherits. A NAME that is already a variable of the shim's environment — an empty
// value included — is refused: the export would land over it with nothing saying so (LEGION-186).
// lookup is the shim's environment.
func ReadProviderEnv(dir string, lookup func(string) (string, bool)) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("--provider-env-dir %s is unreadable: %w", dir, err)
	}
	var env []string
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("--provider-env-dir entry %s is unreadable: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if _, ok := lookup(name + "_FILE"); ok {
			continue
		}
		if _, ok := lookup(name); ok {
			return nil, fmt.Errorf("--provider-env-dir key %s (%s) is already a variable of this shim's environment; "+
				"exporting it would silently override that value — rename or remove the providers-Secret key", name, path)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("--provider-env-dir key %s (%s) is unreadable: %w", name, path, err)
		}
		env = append(env, name+"="+strings.TrimSpace(string(body)))
	}
	return env, nil
}
