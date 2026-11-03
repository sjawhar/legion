package settings

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// The watcher's two periods. A settings change is an operator's act, not a message on a hot path:
// the daemon notices one within a few seconds of the write, and costs the host one read of a
// handful of small directories in between.
const (
	// WatchInterval is how often the watcher reads the directories it watches.
	WatchInterval = 2 * time.Second
	// WatchDebounce is how long a change must stand still before the watcher applies it: an
	// editor that writes a file in several steps, and a kubelet part-way through writing a
	// ConfigMap's new directory, are one change and not three.
	WatchDebounce = 500 * time.Millisecond
)

// Watcher is the file arm of the daemon's settings: it reads the directories holding the
// configuration file and every file a setting names, and settles the configuration again when
// anything in them moves (Keeper.File).
//
// It watches the directories, never the files alone. A Kubernetes ConfigMap mounted as a volume
// is updated by writing a new timestamped directory beside the old one and swapping the `..data`
// symlink: the file the daemon opened is never written, so a watch bound to that file — an
// inotify watch, which binds the inode — would miss every change the operator makes. The
// directory's own entries move at every such swap, and at an ordinary in-place write too.
//
// It reads the directories rather than subscribing to the kernel's events: inotify is Linux's
// alone and reaches this module through no dependency it carries, where a read of a few
// directories every WatchInterval costs the host nothing measurable and behaves the same on
// every host a daemon or a test runs on.
type Watcher struct {
	keeper *Keeper
	log    *slog.Logger
	// Interval and Debounce are WatchInterval and WatchDebounce; a test shortens them.
	Interval time.Duration
	Debounce time.Duration
	// Settled is told what each apply applied, after the apply (nil tells nobody). The daemon
	// hands it the reread of its credential files: a credential rotated in place names no changed
	// setting — its path is what the configuration holds, and the path has not moved — so the
	// file arm is also what asks the holders to read their files again.
	Settled func(cfg *config.Config, applied []config.Change)
	// watched is told after every reading of the watched directories: a test that changes a file
	// must know the watcher has read the state it starts from, since a change made before that
	// reading is part of that state. nil tells nobody.
	watched func()
}

// NewWatcher is the file arm over keeper, at the shipped periods.
func NewWatcher(keeper *Keeper, log *slog.Logger) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{keeper: keeper, log: log, Interval: WatchInterval, Debounce: WatchDebounce}
}

// Run watches until ctx ends. It applies nothing for the state it starts in: that state is what
// the daemon has just booted on.
func (w *Watcher) Run(ctx context.Context) {
	if w.keeper.live.Path() == "" {
		// A configuration the caller resolved itself (config.Settled) has no file to watch.
		return
	}
	held := w.read()
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next := w.read()
		if next == held {
			continue
		}
		// Let the change settle: a write in several steps is one change.
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.Debounce):
			}
			settled := w.read()
			if settled == next {
				break
			}
			next = settled
		}
		held = next
		w.apply(ctx)
	}
}

// apply settles the configuration file as it now reads and reports what changed. A failure is
// logged and nothing moves: the daemon keeps running on the configuration it has, and the next
// change of the file is read again from scratch.
func (w *Watcher) apply(ctx context.Context) {
	applied, err := w.keeper.File(ctx)
	if err != nil {
		w.log.Error("legion settings: the configuration file changed and could not be settled; the daemon keeps the settings it has",
			"path", w.keeper.live.Path(), "error", err)
		return
	}
	if w.Settled != nil {
		w.Settled(w.keeper.live.Snapshot(), applied)
	}
}

// read is the state of every watched directory as one string: each directory's entries by name,
// with the mode, size and modification time of each. A directory that cannot be read is its own
// error text, so a mount that comes and goes is a change like any other.
//
// The set of directories is computed at every read, from the configuration as it now stands, so
// a settings change that names a file somewhere else moves the watch with it.
func (w *Watcher) read() string {
	var state strings.Builder
	for _, dir := range w.directories() {
		state.WriteString(dir)
		state.WriteByte('\n')
		entries, err := os.ReadDir(dir)
		if err != nil {
			state.WriteString("\t" + err.Error() + "\n")
			continue
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				state.WriteString("\t" + entry.Name() + " " + err.Error() + "\n")
				continue
			}
			fmt.Fprintf(&state, "\t%s %s %d %d\n", entry.Name(), info.Mode(), info.Size(), info.ModTime().UnixNano())
		}
	}
	if w.watched != nil {
		w.watched()
	}
	return state.String()
}

// directories is every directory the watcher reads: the one holding the configuration file, and
// one for each file a setting names, each once and in order.
func (w *Watcher) directories() []string {
	cfg := w.keeper.live.Snapshot()
	dirs := []string{filepath.Dir(w.keeper.live.Path())}
	for _, file := range WatchedFiles(cfg) {
		dirs = append(dirs, filepath.Dir(file))
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

// WatchedFiles is every file cfg names that the daemon reads from disk: the credentials it holds
// (the operator, Dispatch and Envoy bearers, the two NATS seeds, the broker login token and the
// cluster credential), the operator's instructions, the provider key files, and the Oh My Pi
// overlays a launch hands a role. Each is watched through the directory holding it.
//
// An App private key is not among them: `github_apps.<app>.private_key` is the key itself, and
// its command and secret sources are no file of this host's.
func WatchedFiles(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	files := []string{
		cfg.OperatorTokenFile, cfg.DispatchTokenFile, cfg.EnvoyTokenFile,
		cfg.NatsNkeySeedFile, cfg.NatsDaemonNkeySeedFile, cfg.InstructionsPath,
	}
	for _, key := range cfg.ProviderKeys {
		files = append(files, key.File)
	}
	for _, overlays := range cfg.RoleConfigFiles {
		files = append(files, overlays...)
	}
	if block := cfg.Runtime.Tmux; block != nil {
		files = append(files, block.PiConfigFiles...)
	}
	if block := cfg.Runtime.Kubernetes; block != nil {
		files = append(files, block.Kubeconfig, block.AgentSecrets.TokenFile)
	}
	named := make([]string, 0, len(files))
	for _, file := range files {
		if file != "" {
			named = append(named, file)
		}
	}
	slices.Sort(named)
	return slices.Compact(named)
}
