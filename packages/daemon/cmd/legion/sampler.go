package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/sampler"
)

const samplerUsage = "usage: legion sampler [--listen 0.0.0.0:9464] [--rules <json>] [--restarts <n>] (--project <key> --issue <key> [--roles-file <path>] [--volumes-file <path>] [--cgroup-root <dir>] | --node [--node-name <node>] [--namespaces <a,b>] [--exclude-namespaces <a,b>] [--exclude-pods <a,b>])"

// samplerModes are the flags of the two modes, each named by the mode it belongs to, so that a
// flag of the other one is refused by name rather than read and quietly ignored. The flags of
// both modes — --listen, --rules and --restarts — are not here.
var samplerModes = []struct {
	flag string
	node bool
}{
	{"project", false},
	{"issue", false},
	{"roles-file", false},
	{"volumes-file", false},
	{"cgroup-root", false},
	{"node-name", true},
	{"namespaces", true},
	{"exclude-namespaces", true},
	{"exclude-pods", true},
}

// runSampler is `legion sampler`, the resource sampler's own process: in an issue pod, which the
// launcher starts it in, it reads that pod's roles and serves each role's CPU and memory by the
// kind of work its processes do; with --node it reads every pod of one node instead. Both serve
// the Prometheus series of docs/sampler-metrics.md on --listen, and both read everything through
// the pod's or the node's own /proc and cgroup filesystem.
//
// It runs until its context ends, each log line on stdout. A failure of its own — a port it
// cannot bind, its anonymous memory past 64 MiB, a collector that refused its options — is one
// line on stderr and exit 1, which the launcher answers by starting it again five seconds later.
func runSampler(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("sampler", samplerUsage, stderr)
	listen := flags.String("listen", fmt.Sprintf("0.0.0.0:%d", sampler.Port), "the address /metrics and /healthz are served on")
	project := flags.String("project", "", "the pod's legion.dev/project label value, which every series carries (an issue pod, required)")
	issue := flags.String("issue", "", "the pod's legion.dev/issue label value, which every series carries (an issue pod, required)")
	rolesFile := flags.String("roles-file", "", "the launcher's roles file, JSON of role name to role-init process id, read again on every pass (an issue pod)")
	volumesFile := flags.String("volumes-file", "", "the pod's volumes file, the JSON list of the volumes to measure (an issue pod)")
	cgroupRoot := flags.String("cgroup-root", "", "the per-command cgroup tree, one cgroup named <role>-<id> for each command; the /proc profile without it (an issue pod)")
	rules := flags.String("rules", "", `the deployment's own kind rules, a JSON list of {"program":"go","arguments":["test"],"kind":"test"}`)
	restarts := flags.Uint64("restarts", 0, "how often the launcher has started the sampler again, which it counts and hands over")
	node := flags.Bool("node", false, "node mode: report every pod of this node rather than this pod's roles")
	nodeName := flags.String("node-name", os.Getenv("NODE_NAME"), "the node this sampler runs on, which a DaemonSet projects as NODE_NAME (node mode, required)")
	namespaces := flags.String("namespaces", "", "the namespaces reported, comma separated; every namespace where empty (node mode)")
	excludeNamespaces := flags.String("exclude-namespaces", "", "namespaces no pod of which is reported, comma separated (node mode)")
	excludePods := flags.String("exclude-pods", "", "pod names that are not reported, comma separated (node mode)")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if len(flags.Args()) != 0 {
		fmt.Fprintln(stderr, samplerUsage)
		return 2
	}
	if code, ok := samplerMode(flags, *node, stderr); !ok {
		return code
	}
	kinds, err := samplerRules(*rules)
	if err != nil {
		fmt.Fprintln(stderr, "legion sampler: "+err.Error())
		return 2
	}
	opts := sampler.Options{Listen: *listen, Log: func(line string) { fmt.Fprintln(stdout, line) }}
	if *node {
		if *nodeName == "" {
			fmt.Fprintln(stderr, "legion sampler: --node-name is required with --node: a DaemonSet projects the node's name as NODE_NAME")
			return 2
		}
		client, err := sampler.NodeClient()
		if err != nil {
			fmt.Fprintln(stderr, "legion sampler: "+err.Error())
			return 1
		}
		registry := sampler.NewNodeRegistry(nil)
		registry.SetRestarts(*restarts)
		opts.Node = &sampler.NodeOptions{
			Root:              "/",
			NodeName:          *nodeName,
			Namespaces:        samplerList(*namespaces),
			ExcludeNamespaces: samplerList(*excludeNamespaces),
			ExcludePods:       samplerList(*excludePods),
			Rules:             kinds,
			Registry:          registry,
			Client:            client,
			Log:               opts.Log,
		}
	} else {
		if *project == "" || *issue == "" {
			fmt.Fprintln(stderr, "legion sampler: --project and --issue are required: every series an issue pod serves carries them")
			return 2
		}
		registry := sampler.NewPodRegistry(*project, *issue, nil)
		registry.SetRestarts(*restarts)
		opts.Pod = &sampler.PodOptions{
			Root:        "/",
			Project:     *project,
			Issue:       *issue,
			RolesFile:   *rolesFile,
			VolumesFile: *volumesFile,
			CgroupRoot:  *cgroupRoot,
			Rules:       kinds,
			Registry:    registry,
			Log:         opts.Log,
		}
	}
	if err := sampler.Run(ctx, opts); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "legion sampler: "+err.Error())
		return 1
	}
	return 0
}

// samplerMode refuses a flag of the mode this sampler is not running, naming it, in the shape
// `legion launcher` refuses --test-postgres without --roles-dir. It reads the flags the command
// line actually set (Visit) and never their values, since --node-name takes its default from the
// NODE_NAME a DaemonSet projects and an issue pod that happens to carry that variable names no
// node by running there.
func samplerMode(flags *flag.FlagSet, node bool, stderr io.Writer) (code int, ok bool) {
	named := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { named[f.Name] = true })
	for _, mode := range samplerModes {
		switch {
		case !named[mode.flag] || mode.node == node:
		case mode.node:
			fmt.Fprintf(stderr, "legion sampler: --%s is a node's: it is refused without --node\n", mode.flag)
			return 2, false
		default:
			fmt.Fprintf(stderr, "legion sampler: --%s is an issue pod's: it is refused with --node\n", mode.flag)
			return 2, false
		}
	}
	return 0, true
}

// samplerRules is the kind engine of this deployment: the JSON list of rules the daemon hands
// over on --rules, refused here where it is not a list of rules or where the engine will not
// take one of them, and the built-in list alone where the flag is empty.
func samplerRules(value string) (sampler.Rules, error) {
	if strings.TrimSpace(value) == "" {
		return sampler.NewRules(nil)
	}
	var operator []sampler.Rule
	if err := json.Unmarshal([]byte(value), &operator); err != nil {
		return sampler.Rules{}, fmt.Errorf(`--rules is a JSON list of {"program":…,"arguments":[…],"kind":…}: %w`, err)
	}
	return sampler.NewRules(operator)
}

// samplerList is one comma-separated flag as the list it names: an empty value is no list at
// all, and the spaces around an entry are not part of it.
func samplerList(value string) []string {
	var list []string
	for _, entry := range strings.Split(value, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			list = append(list, entry)
		}
	}
	return list
}
