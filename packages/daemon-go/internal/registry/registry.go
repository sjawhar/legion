// Package registry is the Go daemon's record of the legions running on this box: which team each
// one coordinates, the configuration it started from, and where to reach it.
//
// The file is the Go daemon's own — `legions-go.json`, never the `legions.json` the shipped
// TypeScript daemon owns until Stage 7 — because the two shapes are not the same record and that
// daemon's reader backs up and resets any file it cannot parse
// (packages/daemon/src/daemon/legions-registry.ts:31-48). The directory it sits in is resolved
// the way the shipped daemon resolves it (packages/daemon/src/daemon/paths.ts:26-37), so both
// registries live side by side under one state home until the rename.
package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// FileName is the registry's name under `<state home>/legion/`.
const FileName = "legions-go.json"

// Entry is one running legion: the team it coordinates, the configuration it was started from —
// which is what `legion restart` starts it again with — and the process to reach or signal.
type Entry struct {
	Team       string    `json:"team"`
	ConfigPath string    `json:"configPath"`
	PID        int       `json:"pid"`
	Port       int       `json:"port"`
	StartedAt  time.Time `json:"startedAt"`
}

// Path is where the registry lives: `$XDG_STATE_HOME/legion/legions-go.json`, and
// `<home>/.local/state/legion/legions-go.json` where that variable is unset or relative — the
// rule the shipped resolver applies (paths.ts:26-31). A nil env reads the process environment.
func Path(env func(string) string, home string) string {
	if env == nil {
		env = os.Getenv
	}
	stateHome := env("XDG_STATE_HOME")
	if !filepath.IsAbs(stateHome) {
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "legion", FileName)
}

// Read is every legion the registry records, in team order. A registry no legion has written yet
// is no legions, not an error; a registry of another shape is refused by name, because this file
// is the Go daemon's alone and a foreign shape in it is a bug to read out loud.
func Read(path string) ([]Entry, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the legions registry at %s: %w", path, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var entries []Entry
	if err := decoder.Decode(&entries); err != nil {
		return nil, fmt.Errorf("read the legions registry at %s: %w", path, err)
	}
	return entries, nil
}

// Find is the entry for one team, and whether the registry has one.
func Find(path, team string) (Entry, bool, error) {
	entries, err := Read(path)
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range entries {
		if e.Team == team {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// Put records a started legion, replacing whatever the team had before: one team runs one legion,
// and a restart is the same team on a new process.
func Put(path string, entry Entry) error {
	return withLock(path, func() error {
		entries, err := Read(path)
		if err != nil {
			return err
		}
		entries = slices.DeleteFunc(entries, func(e Entry) bool { return e.Team == entry.Team })
		entries = append(entries, entry)
		return write(path, entries)
	})
}

// Remove takes a stopped legion out. A team the registry does not carry is not an error: the
// daemon removes its own entry on the way out, and a stop that raced it has nothing left to do.
func Remove(path, team string) error {
	return withLock(path, func() error {
		entries, err := Read(path)
		if err != nil {
			return err
		}
		kept := slices.DeleteFunc(entries, func(e Entry) bool { return e.Team == team })
		if len(kept) == len(entries) {
			return nil
		}
		return write(path, kept)
	})
}

// Alive answers whether the process an entry names is still the legion that wrote it. A pid
// outlives the process that held it — Linux hands the number out again — and `legion stop` and
// `legion restart` signal that pid, so a live process that is not a legion reads as dead. Where
// there is no /proc to ask (a kernel without it, or a process another user owns), the signal is
// the whole answer, as it is in the shipped daemon (legions-registry.ts:14-29).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return true
	}
	return bytes.Contains(cmdline, []byte("legion"))
}

// write replaces the registry by rename, so a reader sees the whole file or the previous one,
// never a half-written list of the legions running on this box.
func write(path string, entries []Entry) error {
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Team, b.Team) })
	if entries == nil {
		entries = []Entry{}
	}
	encoded, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the legions registry: %w", err)
	}
	encoded = append(encoded, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make the legions registry directory %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".legions-go-*.json")
	if err != nil {
		return fmt.Errorf("write the legions registry at %s: %w", path, err)
	}
	defer os.Remove(temp.Name())

	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return fmt.Errorf("write the legions registry at %s: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write the legions registry at %s: %w", path, err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("replace the legions registry at %s: %w", path, err)
	}
	return nil
}

// withLock serializes the read-modify-write, because two legions starting at once are two
// processes rewriting one file and the loser's entry would be the one that vanished. The lock is
// its own file: the registry's inode changes on every write, so a lock held on it would lock
// nothing. A process that dies holding the lock releases it — the kernel closes the descriptor.
func withLock(path string, change func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make the legions registry directory %s: %w", dir, err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open the legions registry lock for %s: %w", path, err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock the legions registry at %s: %w", path, err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	return change()
}
