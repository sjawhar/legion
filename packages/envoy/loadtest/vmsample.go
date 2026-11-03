package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// vmSampleScript is vm/sample.sh, the script every sampled VM runs. The collector sends it on
// ssh's standard input (sh -s), as cluster.sh sends SAMPLE_SCRIPT through kubectl exec
// (cluster/cluster.sh:1046-1056), so a VM needs nothing installed and the script a run samples
// with is the one in this tree.
//
//go:embed vm/sample.sh
var vmSampleScript string

// vmSampleSilence is how long a VM's ssh may print nothing before the collector takes the stream
// for dead and starts another: six sampling intervals at the 5 s default, the same 30 s that
// cluster.sh's parse_samples waits (cluster/cluster.sh:1111).
const vmSampleSilence = 30 * time.Second

// vmSampleStderr is how much of a failed ssh's standard error the error reading carries, as
// cluster.sh's sample_role_once keeps its last 400 bytes.
const vmSampleStderr = 400

// vmCgroupRoot is where a role with no cgroup directory of its own is read, as vm/sample.sh
// defaults it.
const vmCgroupRoot = "/sys/fs/cgroup"

// vmRole is one container sampled on a VM: the role its samples carry, and the cgroup v2
// directory its counters live in.
type vmRole struct {
	name string
	dir  string
}

// vmTarget is one sampled VM: the ssh target, the host that names its samples' pod, the role of
// the machine's own series, and the containers to watch on it.
type vmTarget struct {
	ssh         string
	host        string
	machineRole string
	roles       []vmRole
}

// vmMachineRolePrefix starts the role of a VM's own series (vm-<host>), so a reader of
// cgroup.json tells the machines from the containers by name, as the NATS pods' nats- prefix
// does.
const vmMachineRolePrefix = "vm-"

// parseVMTarget reads one -vm value, <user@host:role[=cgroup dir][,role...]>. The machine's own
// series takes the role vm-<host>, so the VM that runs the generator is a series like any other.
func parseVMTarget(spec string) (*vmTarget, error) {
	target, roles, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("-vm %q names no role: write <user@host:role[=cgroup dir][,role...]>", spec)
	}
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("-vm %q names no ssh target", spec)
	}
	host := target
	if _, after, found := strings.Cut(target, "@"); found {
		host = after
	}
	if host == "" {
		return nil, fmt.Errorf("-vm %q names no host after its user", spec)
	}
	t := &vmTarget{ssh: target, host: host, machineRole: vmMachineRolePrefix + host}
	for _, field := range strings.Split(roles, ",") {
		name, dir, hasDir := strings.Cut(field, "=")
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("-vm %q has an empty role", spec)
		}
		if !hasDir {
			dir = path.Join(vmCgroupRoot, name)
		}
		if dir = strings.TrimSpace(dir); dir == "" {
			return nil, fmt.Errorf("-vm %q gives role %s an empty cgroup directory", spec, name)
		}
		if name == t.machineRole {
			return nil, fmt.Errorf("-vm %q names a role %s, which is the role of the machine's own series", spec, name)
		}
		for _, r := range t.roles {
			if r.name == name {
				return nil, fmt.Errorf("-vm %q names the role %s twice", spec, name)
			}
		}
		t.roles = append(t.roles, vmRole{name: name, dir: dir})
	}
	return t, nil
}

// vmSeries is one series a target writes: the role its samples carry and the container name on
// the VM behind it, "vm" for the machine itself.
type vmSeries struct {
	role      string
	container string
}

// series are every series this target writes, the machine's own last; the error reading of a
// stream that died names each of them, so no series closes silently.
func (t *vmTarget) series() []vmSeries {
	out := make([]vmSeries, 0, len(t.roles)+1)
	for _, r := range t.roles {
		out = append(out, vmSeries{role: r.name, container: path.Base(r.dir)})
	}
	return append(out, vmSeries{role: t.machineRole, container: "vm"})
}

// knows is true for a role this target asked the script for; the script echoes the role of every
// reading back, so a reading of another role is a stream the collector does not understand.
func (t *vmTarget) knows(role string) bool {
	if role == t.machineRole {
		return true
	}
	for _, r := range t.roles {
		if r.name == role {
			return true
		}
	}
	return false
}

// scriptArgs are vm/sample.sh's arguments for this target at the collector's interval.
func (t *vmTarget) scriptArgs(interval time.Duration) []string {
	args := []string{"-i", strconv.FormatFloat(interval.Seconds(), 'f', -1, 64), "-m", t.machineRole}
	for _, r := range t.roles {
		args = append(args, r.name+"="+r.dir)
	}
	return args
}

// vmTargets is the repeatable -vm flag.
type vmTargets []*vmTarget

func (ts *vmTargets) String() string {
	out := make([]string, 0, len(*ts))
	for _, t := range *ts {
		out = append(out, t.ssh)
	}
	return strings.Join(out, " ")
}

func (ts *vmTargets) Set(value string) error {
	t, err := parseVMTarget(value)
	if err != nil {
		return err
	}
	for _, prev := range *ts {
		if prev.machineRole == t.machineRole {
			return fmt.Errorf("-vm %s names host %s twice", value, t.host)
		}
		for _, r := range t.roles {
			if prev.knows(r.name) {
				return fmt.Errorf("-vm %s names the role %s, which %s already samples: a role is one series in cgroup.json", value, r.name, prev.ssh)
			}
		}
	}
	*ts = append(*ts, t)
	return nil
}

// vmSampler collects the readings of every sampled VM into one cgroup.jsonl. Every line is
// stamped with this collector's clock, never a VM's, so one clock orders every series, as
// cluster.sh's worker stamps the readings of every container it samples.
type vmSampler struct {
	ssh      []string
	interval time.Duration
	now      func() time.Time
	lg       *log.Logger

	mu      sync.Mutex
	out     io.Writer
	written int
	failed  int
}

// write appends one sample as a cgroup.jsonl line; a line that cannot be written is logged, not
// returned, so one failed write does not end the run's sampling.
func (s *vmSampler) write(sample cgroupSample) {
	b, err := json.Marshal(sample)
	if err != nil {
		s.lg.Printf("marshal the %s sample: %v", sample.Role, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.out.Write(append(b, '\n')); err != nil {
		s.lg.Printf("write the %s sample: %v", sample.Role, err)
		return
	}
	s.written++
	if !sample.good() {
		s.failed++
	}
}

// vmErrorSample is a failed read: the error, and no number, in the shape cluster.sh's emit_error
// writes (cluster/cluster.sh:1097-1105).
func vmErrorSample(at time.Time, role, pod, container, msg string) cgroupSample {
	return cgroupSample{At: at, Role: role, Pod: pod, Container: container, Error: vmOneLine(msg)}
}

// vmOneLine is msg as one line of at most vmSampleStderr bytes, its last, which is where an ssh
// says why it ended.
func vmOneLine(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > vmSampleStderr {
		msg = "…" + msg[len(msg)-vmSampleStderr:]
	}
	return msg
}

// vmReading is one block of vm/sample.sh's output: its "key value" lines up to "end".
type vmReading struct {
	role      string
	container string
	usage     *int64
	throttled *int64
	cpuMax    string
	memory    *int64
	inactive  *int64
	errs      []string
}

// sample is the cgroup.jsonl line this reading writes, stamped at: the numbers when the VM read
// every one of them and reported no unreadable file, and a failed read otherwise, never a zero.
func (rd *vmReading) sample(at time.Time, pod string) cgroupSample {
	s := cgroupSample{At: at, Role: rd.role, Pod: pod, Container: rd.container}
	if len(rd.errs) == 0 && rd.usage != nil && rd.throttled != nil && rd.memory != nil && rd.inactive != nil && vmCPUMaxOK(rd.cpuMax) {
		s.UsageUsec, s.ThrottledUsec, s.CPUMax, s.MemoryCurrent, s.InactiveFile = rd.usage, rd.throttled, rd.cpuMax, rd.memory, rd.inactive
		return s
	}
	why := strings.Join(rd.errs, "; ")
	if why == "" {
		why = fmt.Sprintf("usage_usec=%s throttled_usec=%s cpu_max=%q memory_current=%s inactive_file=%s",
			vmNumber(rd.usage), vmNumber(rd.throttled), rd.cpuMax, vmNumber(rd.memory), vmNumber(rd.inactive))
	}
	s.Error = vmOneLine("incomplete read: " + why)
	return s
}

// vmNumber is a read number, or "missing".
func vmNumber(p *int64) string {
	if p == nil {
		return "missing"
	}
	return strconv.FormatInt(*p, 10)
}

// vmCPUMaxOK is true for a cgroup v2 cpu.max, "<quota> <period>" or "max <period>", which the
// cluster sampler checks the same way before it writes a good line
// (cluster/cluster.sh:1116-1117): a container with no CPU limit reads "max", and that is a read
// that worked, not a failed one.
func vmCPUMaxOK(text string) bool {
	quota, period, ok := strings.Cut(text, " ")
	if !ok || (quota != "max" && !vmWholeNumber(quota)) {
		return false
	}
	return vmWholeNumber(period)
}

// vmWholeNumber is true for a non-empty run of digits.
func vmWholeNumber(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// parseVMReadings turns vm/sample.sh's blocks on r into one sample per block, each stamped with
// now() when its "end" arrived and emitted at once, so a long-lived stream writes as it reads.
// It returns when the stream ends, or an error for a block the collector cannot place: one that
// names no role, or a role this target never asked for.
func parseVMReadings(r io.Reader, t *vmTarget, now func() time.Time, emit func(cgroupSample)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var rd vmReading
	number := func(key, value string) *int64 {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			rd.errs = append(rd.errs, fmt.Sprintf("%s is not a number: %q", key, value))
			return nil
		}
		return &n
	}
	for sc.Scan() {
		key, value, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		switch key {
		case "":
		case "role":
			rd.role = value
		case "container":
			rd.container = value
		case "usage_usec":
			rd.usage = number(key, value)
		case "throttled_usec":
			rd.throttled = number(key, value)
		case "cpu_max":
			rd.cpuMax = value
		case "memory_current":
			rd.memory = number(key, value)
		case "inactive_file":
			rd.inactive = number(key, value)
		case "error":
			rd.errs = append(rd.errs, value)
		case "end":
			switch {
			case rd.role == "":
				return fmt.Errorf("a reading of %s names no role", t.ssh)
			case !t.knows(rd.role):
				return fmt.Errorf("a reading of %s names the role %s, which it was not asked to sample", t.ssh, rd.role)
			}
			emit(rd.sample(now(), t.host))
			rd = vmReading{}
		default:
			rd.errs = append(rd.errs, fmt.Sprintf("the VM printed %q", vmOneLine(sc.Text())))
		}
	}
	return sc.Err()
}

// stream holds one ssh to the target, running vm/sample.sh, and writes every reading it prints.
// It returns why the stream ended: an ssh that dies, a VM that stops printing for
// vmSampleSilence, or ctx, which is the collector stopping.
func (s *vmSampler) stream(ctx context.Context, t *vmTarget) error {
	args := append(append([]string{}, s.ssh[1:]...), t.ssh, "sh", "-s", "--")
	cmd := exec.CommandContext(ctx, s.ssh[0], append(args, t.scriptArgs(s.interval)...)...)
	cmd.Stdin = strings.NewReader(vmSampleScript)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
	}
	// A VM whose script is wedged prints nothing and its ssh never ends, so the stream is killed
	// once it has been silent for vmSampleSilence and started again, as any dead one is.
	silent := false
	idle := time.AfterFunc(vmSampleSilence, func() {
		silent = true
		_ = cmd.Process.Kill()
	})
	defer idle.Stop()
	parseErr := parseVMReadings(stdout, t, s.now, func(sample cgroupSample) {
		idle.Reset(vmSampleSilence)
		s.write(sample)
	})
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case silent:
		return fmt.Errorf("the VM printed nothing for %s", seconds(vmSampleSilence))
	case parseErr != nil:
		return parseErr
	case waitErr != nil:
		return fmt.Errorf("the ssh ended: %v: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return fmt.Errorf("the ssh ended with no error: %s", strings.TrimSpace(stderr.String()))
}

// follow keeps one ssh to the target open until ctx ends. A stream that died leaves an error
// reading of every series on the VM, never a silently closed series, and the next ssh starts
// after one interval, so a VM that refuses every connection is not dialled in a tight loop.
func (s *vmSampler) follow(ctx context.Context, t *vmTarget) {
	for ctx.Err() == nil {
		err := s.stream(ctx, t)
		if ctx.Err() != nil {
			return
		}
		at := s.now()
		for _, r := range t.series() {
			s.write(vmErrorSample(at, r.role, t.host, r.container, fmt.Sprintf("the sampler's ssh to %s ended, so this series has a gap: %v", t.ssh, err)))
		}
		s.lg.Printf("%s: %v; another ssh in %s", t.ssh, err, seconds(s.interval))
		sleepUntil(ctx, s.now().Add(s.interval))
	}
}

// run samples every target until ctx ends.
func (s *vmSampler) run(ctx context.Context, targets []*vmTarget) {
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.follow(ctx, t)
		}()
	}
	wg.Wait()
}

func runVMSample(args []string) error {
	fs := flag.NewFlagSet("vm-sample", flag.ExitOnError)
	out := fs.String("out", "", "the cgroup.jsonl to append every sample to (required)")
	var targets vmTargets
	fs.Var(&targets, "vm", "a sampled VM, <user@host:role[=cgroup dir][,role...]>; repeatable. A role with no directory is read at "+vmCgroupRoot+"/<role>, and the machine's own series takes the role "+vmMachineRolePrefix+"<host>")
	sshCmd := fs.String("ssh", "ssh", "the command that opens a shell on a VM, its arguments separated by spaces")
	interval := fs.Duration("interval", 5*time.Second, "how often each VM prints a reading")
	until := fs.Duration("until", 0, "stop this long after the start; 0 samples until a signal")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen vm-sample -out <cgroup.jsonl> -vm <user@host:role[=cgroup dir][,role...]>... [-ssh ssh] [-interval 5s] [-until 0]")
		fmt.Fprintln(fs.Output(), "Holds one ssh per VM running vm/sample.sh and appends every reading as a cgroup.jsonl line,")
		fmt.Fprintln(fs.Output(), "the file loadgen cgroup-merge reads. It stops on SIGINT or SIGTERM, and at -until.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	sshFields := strings.Fields(*sshCmd)
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case *out == "":
		return errors.New("-out is required: the cgroup.jsonl this run's samples are appended to")
	case len(targets) == 0:
		return errors.New("-vm is required: name each VM and the roles on it, <user@host:role[=cgroup dir][,role...]>")
	case len(sshFields) == 0:
		return errors.New("-ssh is required: the command that opens a shell on a VM")
	case *interval <= 0:
		return fmt.Errorf("-interval must be above 0, not %s", *interval)
	case *until < 0:
		return fmt.Errorf("-until must be 0 or more, not %s", *until)
	}
	f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *until > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *until)
		defer cancel()
	}
	s := &vmSampler{
		ssh:      sshFields,
		interval: *interval,
		now:      time.Now,
		lg:       log.New(os.Stderr, "loadgen vm-sample: ", log.LstdFlags),
		out:      f,
	}
	roles := 0
	for _, t := range targets {
		roles += len(t.series())
	}
	s.lg.Printf("sampling %d roles on %d VMs every %s into %s", roles, len(targets), seconds(*interval), *out)
	s.run(ctx, targets)
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(os.Stderr, "loadgen vm-sample: wrote %d samples to %s (%d failed reads)\n", s.written, *out, s.failed)
	return nil
}
