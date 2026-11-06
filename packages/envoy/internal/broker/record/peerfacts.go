package record

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// maxPeerPath bounds a path or a cgroup line in a peer claim at PATH_MAX, the longest path Linux
// resolves and so the longest /proc can answer. It bounds what an approval page renders.
const maxPeerPath = 4096

// PeerFacts is what the kernel answered about the process that asked for a credential, read by
// the agent-secrets helper through the pidfd it had pinned on its own unix socket and signed
// into the request object.
//
// These are answers, not statements. A request's session id (the broker's requests.session_id)
// and its reason are what the asking process says about itself, in a field it fills; these are
// what the kernel says about that same process, at ReadAt. A page that renders both must keep
// them apart, because they are worth different amounts of trust.
//
// How far that trust reaches: the claim is signed with the enrolled session's key, and the only
// signer in this module that makes one is the helper, from its own reading. On a machine session
// the helper runs, the asking process never holds that key, so it can neither write this claim
// nor change it. A session whose key lives beside the agent instead — a box's key.pem — could
// sign values it chose, and nothing here would tell. A pod, which has no helper at all, is
// refused outright (requests.Machine.Create).
type PeerFacts struct {
	// The pid the pinned handle named when the helper read these facts.
	PID int `json:"pid"`
	// /proc/<pid>/exe, verbatim: the binary the kernel resolved for this process, with the
	// " (deleted)" suffix the kernel adds when that file has been replaced or removed since
	// exec. A process chooses what it execs, and chooses its argv[0] and its comm freely; this
	// is the kernel's answer about what it actually runs, which argv[0] and comm are not.
	Executable string `json:"executable"`
	// /proc/<pid>/cwd, verbatim: the directory the process is working in. A process chooses its
	// working directory; this is the kernel's answer about the one it is in.
	WorkingDir string `json:"working_dir"`
	// The unified hierarchy's line of /proc/<pid>/cgroup ("0::" and the path) with its newline
	// removed, or the file's first line on a host that gives the process no unified hierarchy:
	// the cgroup its host's manager placed it in, which under systemd is the scope or service it
	// runs in.
	Cgroup string `json:"cgroup"`
	// When the kernel says the process started, in Unix seconds: /proc/<pid>/stat's starttime
	// (ticks of the kernel's USER_HZ since boot) added to /proc/stat's btime.
	StartedAt int64 `json:"started_at"`
	// When the helper read these facts through the pinned handle, in Unix seconds. A fact without
	// the instant it was read is a fact a reader cannot age: a working directory read two hours
	// ago is honest only while it says when it was read.
	ReadAt int64 `json:"read_at"`
}

// ValidPeerFacts bounds the shape of a peer claim: a positive pid, absolute paths, three
// strings no longer than a path and free of the rune categories validateReason refuses, and a
// read at or after the start it names. It proves nothing about how the values were obtained —
// no check here can — and is the same kind of bound validateReason puts on the reason: what an
// approval page may be asked to render, and what the broker may be asked to store.
//
// The helper runs it over its own reading before it signs, because a claim the broker refuses
// would refuse the whole request with it: a working directory with a zero-width joiner in its
// name is no reason to leave a session unable to ask for a secret.
func ValidPeerFacts(f PeerFacts) error {
	if f.PID <= 0 {
		return errors.New("peer pid is not a pid")
	}
	for _, field := range []struct{ name, value string }{
		{"executable", f.Executable},
		{"working_dir", f.WorkingDir},
	} {
		if !strings.HasPrefix(field.value, "/") {
			return fmt.Errorf("peer %s is not an absolute path", field.name)
		}
	}
	for _, field := range []struct{ name, value string }{
		{"executable", f.Executable},
		{"working_dir", f.WorkingDir},
		{"cgroup", f.Cgroup},
	} {
		if field.value == "" {
			return fmt.Errorf("peer %s is empty", field.name)
		}
		if len(field.value) > maxPeerPath {
			return fmt.Errorf("peer %s is over %d bytes", field.name, maxPeerPath)
		}
		for _, r := range field.value {
			if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs, unicode.Co, unicode.Zl, unicode.Zp) {
				return fmt.Errorf("peer %s contains a refused rune category", field.name)
			}
		}
	}
	switch {
	case f.StartedAt <= 0:
		return errors.New("peer started_at is not an instant")
	case f.ReadAt <= 0:
		return errors.New("peer read_at is not an instant")
	case f.ReadAt < f.StartedAt:
		return errors.New("peer read_at is before started_at")
	}
	return nil
}
