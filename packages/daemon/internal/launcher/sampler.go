package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/sampler"
)

// The pod's resource sampler, which an issue pod's launcher runs as a child of its own when the
// daemon passes --sampler (Config.Sampler): `legion sampler`, this binary's own command, serving
// the pod's Prometheus series on SamplerConfig.Port (docs/sampler-metrics.md). SamplerDirName is
// the directory under the roles directory holding the two files the launcher writes it, which
// every role's namespace covers (role-init), so no role can rewrite the map of pids the sampler
// reads its roles from.
//
// The launcher runs it and no role does, for the reason it runs the test database: a sampler a
// role's generation started would die with that generation, and it must read every role's
// processes rather than one role's. It is the launcher's child and not a container of its own
// because it reads the pid of each role's generation, which only the process that forked them
// knows, and the /proc of the pod's own namespace.
const (
	// SamplerDirName is the sampler's own directory under the roles directory.
	SamplerDirName = "sampler"
	// samplerRolesName is the map of role name to that role's running generation's role-init
	// process id, rewritten whenever one of them changes, and samplerVolumesName the list of
	// volumes the pod mounts, written once at the start.
	samplerRolesName   = "roles.json"
	samplerVolumesName = "volumes.json"
	// samplerCommand is this binary's command the launcher runs, and samplerProgram the prefix
	// its output carries in the container's log.
	samplerCommand = "sampler"
	samplerProgram = "legion-sampler"
)

// SamplerConfig is the pod's sampler as the daemon hands it over: the port it serves on, the
// project and issue every series it serves carries, which the launcher is told because it knows
// neither, the volumes to measure and the deployment's own kind rules. It is one JSON flag
// (`legion launcher --sampler <json>`) and not a flag for each field, since a list of volumes is
// no repeated scalar flag, and it is exported because the daemon's pod builder renders it.
type SamplerConfig struct {
	Port    int              `json:"port"`
	Project string           `json:"project"`
	Issue   string           `json:"issue"`
	Volumes []sampler.Volume `json:"volumes"`
	Rules   []sampler.Rule   `json:"rules,omitempty"`
}

// samplerInterval is how long the launcher waits before it starts the sampler again. The sampler
// runs until the container ends, so an exit is a failure of its own — a port it cannot bind yet,
// a collector that refused its options, a kill under memory pressure — and the pause keeps a
// sampler that cannot run at all from spinning on the pod's CPU, which is the resource it is
// there to report. A test shortens it.
var samplerInterval = 5 * time.Second

// samplerDir is the sampler's directory for this launcher's roles directory, and the two files
// in it that the launcher writes and the sampler reads.
func samplerDir(rolesDir string) string { return filepath.Join(rolesDir, SamplerDirName) }

func samplerRolesPath(rolesDir string) string {
	return filepath.Join(samplerDir(rolesDir), samplerRolesName)
}

func samplerVolumesPath(rolesDir string) string {
	return filepath.Join(samplerDir(rolesDir), samplerVolumesName)
}

// serveSampler writes the pod's volumes file and keeps the sampler running until ctx ends, and
// answers a channel closed once its goroutine has returned, so a caller that ends ctx can wait
// for it rather than leave it running. The volumes file is written here, before any role starts,
// because it is what the configuration carried and never changes while the pod lives; the roles
// file is written by every generation that starts and ends (samplerRoles).
//
// A sampler that cannot start, or that exits at once, is logged and started again rather than
// thrown: a pod whose readings are missing still runs every role it serves, where a launcher that
// failed its container would cost the issue all of them.
func serveSampler(ctx context.Context, cfg Config, log *roleLog, guard *pidGuard) <-chan struct{} {
	// The interval is read here, in the caller's own goroutine, and not inside the one below: it
	// is a package variable a test shortens, and a goroutine still reading it after the test
	// restored it is a data race the race detector fails the test for.
	interval := samplerInterval
	if err := writeSamplerFile(samplerVolumesPath(cfg.RolesDir), cfg.Sampler.Volumes); err != nil {
		log.linef("launcher: sampler volumes file: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for restarts := uint64(0); ; restarts++ {
			if err := runSampler(ctx, cfg, log, guard, restarts); err != nil {
				log.linef("launcher: sampler: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
	return done
}

// runSampler runs one sampler until it exits, writing its output to the container's log behind a
// prefix of its own, so its lines read apart from a role's. Its process is held out of the
// reaper's pass (pidGuard) until this wait has read its exit. restarts is how often the launcher
// has started it again, which the sampler serves as its own restart counter: the series would
// otherwise say nothing about a sampler that keeps dying and being replaced.
func runSampler(ctx context.Context, cfg Config, log *roleLog, guard *pidGuard, restarts uint64) error {
	argv, err := samplerArgv(cfg, restarts)
	if err != nil {
		return err
	}
	out, _ := log.writers(samplerProgram)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Stdout, cmd.Stderr = out, out
	pid, err := guard.start(cmd)
	if err != nil {
		return fmt.Errorf("start %s: %w", samplerProgram, err)
	}
	defer guard.done(pid)
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("%s: %w", samplerProgram, err)
	}
	return nil
}

// samplerExecutable is this binary, which the launcher runs the sampler as its own command of. A
// test replaces it with a stub that records the command line it was given, since the test binary
// is not `legion`.
var samplerExecutable = os.Executable

// samplerArgv is the command line of one sampler: this binary's `sampler` command over the pod
// the launcher serves. The two files are named by path rather than by the roles directory,
// because the sampler takes each of them on a flag of its own and a node's sampler has neither.
func samplerArgv(cfg Config, restarts uint64) ([]string, error) {
	self, err := samplerExecutable()
	if err != nil {
		return nil, fmt.Errorf("find this binary: %w", err)
	}
	port := cfg.Sampler.Port
	if port == 0 {
		port = sampler.Port
	}
	argv := []string{
		self, samplerCommand,
		"--listen", fmt.Sprintf("0.0.0.0:%d", port),
		"--project", cfg.Sampler.Project,
		"--issue", cfg.Sampler.Issue,
		"--roles-file", samplerRolesPath(cfg.RolesDir),
		"--volumes-file", samplerVolumesPath(cfg.RolesDir),
	}
	if len(cfg.Sampler.Rules) > 0 {
		rules, err := json.Marshal(cfg.Sampler.Rules)
		if err != nil {
			return nil, fmt.Errorf("the deployment's kind rules: %w", err)
		}
		argv = append(argv, "--rules", string(rules))
	}
	return append(argv, "--restarts", strconv.FormatUint(restarts, 10)), nil
}

// samplerRoles is the sampler's roles file: each role whose generation is running and that
// generation's role-init process id, which is the root of the subtree the sampler counts as the
// role's. Every manager of the launcher writes its own role's entry to the one file, so it has a
// lock of its own: a manager holds its own lock while its generation's process id changes and
// takes this one inside it, and no path takes them the other way around.
//
// A write that fails is logged and nothing else, as every other failure of the sampler's is: the
// readings of a pod whose file could not be written are wrong for a pass, and its roles run.
type samplerRoles struct {
	path string
	log  *roleLog
	mu   sync.Mutex
	pids sampler.Roles
}

// newSamplerRoles is the file as a launcher that has started no generation has it: a map with no
// role in it, written before any role connects, so the sampler's first pass reads a pod with no
// generation running rather than a file that is not there.
func newSamplerRoles(rolesDir string, log *roleLog) *samplerRoles {
	roles := &samplerRoles{path: samplerRolesPath(rolesDir), log: log, pids: sampler.Roles{}}
	roles.mu.Lock()
	defer roles.mu.Unlock()
	roles.rewrite()
	return roles
}

// set records the process id of role's running generation. A nil receiver is a launcher serving
// no sampler, which every call site has rather than a condition of its own.
func (r *samplerRoles) set(role string, pid int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pids[role] = pid
	r.rewrite()
}

// clear forgets role's generation once it has ended, so the sampler stops looking for a process
// id the kernel may already have given to something else.
func (r *samplerRoles) clear(role string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pids, role)
	r.rewrite()
}

// rewrite writes the whole map as it stands. The caller holds the lock.
func (r *samplerRoles) rewrite() {
	if err := writeSamplerFile(r.path, r.pids); err != nil {
		r.log.linef("launcher: sampler roles file: %v", err)
	}
}

// writeSamplerFile writes one of the sampler's two files whole: JSON to a temporary file in the
// directory the file belongs in, and then a rename over the name. A sampler reading the file as
// it is written would otherwise catch half of it, which it counts as a failed pass and reports as
// such, and the roles file is rewritten at every start and end of a generation.
func writeSamplerFile(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = temp.Write(append(raw, '\n'))
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(temp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(temp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(temp.Name())
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
