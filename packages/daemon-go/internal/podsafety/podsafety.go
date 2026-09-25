// Package podsafety is the baseline a Legion pod's Oh My Pi starts with, and nothing in it names a
// model, a provider, or a route: a settings overlay that holds off the endpoints Oh My Pi posts a
// conversation to on its own, and four variables through which a repository's .env would move
// what the agent runs with. Everything here yields to the operator's own configuration
// (runtime.kubernetes.pod): the overlay is named first, before the operator's overlays, and a
// variable the pod's environment sets is kept. It runs only in a pod (`legion worker-shim
// --pod-safety`, `legion probe-image --pod-safety`); a tmux pane starts Oh My Pi as it always has.
package podsafety

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// overlay is the settings overlay Apply writes (overlay.yml, which says what each setting holds off).
//
//go:embed overlay.yml
var overlay []byte

// OverlayFile is the overlay's file under the state directory Apply is given.
const OverlayFile = "podsafety-overlay.yml"

// settingsOverlays is Oh My Pi's list of settings overlays, PATH-separated: each outranks the
// repository's .omp/config.yml, and a later one outranks an earlier one.
const settingsOverlays = "PI_CONFIG_FILES"

// baseline are the variables Apply sets where the pod's environment leaves them unset or empty,
// which is when Oh My Pi would fill them from the working directory's .env.
var baseline = []struct{ name, value string }{
	// The OpenTelemetry SDK exports logs, traces, and metrics to OTEL_EXPORTER_OTLP_ENDPOINT.
	{"OTEL_SDK_DISABLED", "true"},
	// Outranks dev.autoqa, which pushes tool-issue reports.
	{"PI_AUTO_QA", "0"},
	// Names the config root Oh My Pi's agent directory is joined under (.omp in the image), which
	// chooses the models.yml and profile it reads: no settings overlay outranks it.
	{"PI_CONFIG_DIR", ".omp"},
	// Outranks session.storage, and with OMP_SESSION_SQL_DSN_FILE would write the conversation to
	// a database the repository names.
	{"OMP_SESSION_STORAGE", "file"},
}

// Variables are the variables Apply sets: the settings overlays it composes, and each baseline
// variable.
func Variables() []string {
	names := []string{settingsOverlays}
	for _, v := range baseline {
		names = append(names, v.name)
	}
	return names
}

// Apply is environ as a pod's Oh My Pi starts with it: the overlay written read-only to
// <stateDir>/podsafety-overlay.yml and named first in PI_CONFIG_FILES, ahead of the overlays
// environ already names, and each baseline variable set where environ leaves it unset or empty.
func Apply(environ []string, stateDir string) ([]string, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("pod safety: no state directory to write %s to", OverlayFile)
	}
	file := filepath.Join(stateDir, OverlayFile)
	if err := writeReadOnly(file, overlay); err != nil {
		return nil, fmt.Errorf("pod safety: write %s: %w", file, err)
	}
	overlays := file
	out := make([]string, 0, len(environ)+len(baseline)+1)
	set := map[string]bool{}
	for _, pair := range environ {
		name, value, _ := strings.Cut(pair, "=")
		switch {
		case name == settingsOverlays:
			if value != "" {
				overlays += string(os.PathListSeparator) + value
			}
			continue
		case value != "":
			set[name] = true
		}
		out = append(out, pair)
	}
	for _, v := range baseline {
		if !set[v.name] {
			out = append(removed(out, v.name), v.name+"="+v.value)
		}
	}
	return append(out, settingsOverlays+"="+overlays), nil
}

// removed is environ without its entries for name.
func removed(environ []string, name string) []string {
	kept := environ[:0]
	for _, pair := range environ {
		if !strings.HasPrefix(pair, name+"=") {
			kept = append(kept, pair)
		}
	}
	return kept
}

// writeReadOnly writes body to file at mode 0444, through a temporary file renamed into place, so
// a file an earlier start left, read-only, is replaced rather than refusing the write.
func writeReadOnly(file string, body []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o444); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), file)
}
