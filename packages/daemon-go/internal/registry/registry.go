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
// which is what `legion restart` starts it again with — and the address and process to reach or
// signal. It is the daemon's whole claim on the team: there is no second record (no pid file)
// that could disagree with it, the way the shipped daemon's pid file did before LEGION-35
// (docs/solutions/daemon/instance-lock-is-a-kernel-flock-not-a-pid-file.md).
type Entry struct {
	Team       string    `json:"team"`
	ConfigPath string    `json:"configPath"`
	PID        int       `json:"pid"`
	Port       int       `json:"port"`
	Bind       string    `json:"bind"`
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

// Claim records a starting legion, and only while no live process holds the team. The check and
// the write are one critical section: two starts that read, checked and then wrote would both
// pass the check, and the loser's release would delete the winner's claim — leaving a serving
// daemon with no record at all. Returns the entry that holds the team and false when one does,
// and the caller's own entry and true when it took it. A team whose entry names a pid nothing
// holds is reclaimed, which is how a crashed daemon's record is replaced.
//
// The read-modify-write is inlined rather than delegated: `withLock` is a flock per call and
// flock is per open file description, so a Claim that took the lock and called another locking
// function would wait for itself.
func Claim(path string, entry Entry) (Entry, bool, error) {
	var held Entry
	var mine bool
	err := withLock(path, func() error {
		entries, err := Read(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Team == entry.Team && e.PID != entry.PID && Alive(e.PID) {
				held, mine = e, false
				return nil
			}
		}
		entries = slices.DeleteFunc(entries, func(e Entry) bool { return e.Team == entry.Team })
		held, mine = entry, true
		return write(path, append(entries, entry))
	})
	if err != nil {
		return Entry{}, false, err
	}
	return held, mine, nil
}

// RemoveIf takes a stopped legion out, but only while the entry still names pid. A team the
// registry does not carry is not an error: the daemon removes its own entry on the way out, and
// a stop that raced it has nothing left to do. The pid test is what keeps a daemon that failed
// to start from releasing the claim of the one that is serving.
func RemoveIf(path, team string, pid int) error {
	return withLock(path, func() error {
		entries, err := Read(path)
		if err != nil {
			return err
		}
		kept := slices.DeleteFunc(entries, func(e Entry) bool { return e.Team == team && e.PID == pid })
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
	// argv[0]'s own name, not the whole command line: this checkout is /home/ubuntu/src/legion,
	// so a `go build`, an editor or a jj invocation carries "legion" in its arguments, and a
	// recycled pid belonging to one of them would take the SIGTERM `legion stop` sends.
	argv0, _, _ := bytes.Cut(cmdline, []byte{0})
	return filepath.Base(string(argv0)) == "legion"
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
