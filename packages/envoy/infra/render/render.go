// Package render is `envoy-infra render`: it prints what the AWS components register, from the
// same inputs, through the same functions the components build it with, so a program that needs
// one of those renderings outside Pulumi reads it here rather than rebuilding it. Each flag sets a
// component input, and a value the component refuses is refused here, naming the flag.
package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sjawhar/envoy/infra/aws/rendering"
	"github.com/sjawhar/envoy/infra/core"
)

const usageText = `usage: envoy-infra render {nats-start-script|webhook-listener-env|metric-filters} [flags]

  nats-start-script     the script aws:NatsService's NATS container runs under /bin/sh -c when it
                        has an authorization, exactly as the component registers it, with no
                        newline after it
  webhook-listener-env  aws:WebhookListener's non-secret container environment, one KEY=value line
                        per variable in the container's order; the secrets ECS injects are not printed
  metric-filters        a component's log metric filters as a JSON array: each one's logicalName,
                        pattern, metricName and metricNamespace

Each flag's help names the component input it sets, and a value the component refuses is refused
here, naming the flag. "envoy-infra render <subcommand> --help" lists a subcommand's flags. Exit
codes: 0 printed; 2 refused, as "refused: --<flag>: <reason>", or a usage error.

`

// stringList is a flag given once per value, kept in the order given.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, " ") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// subcommand is one subcommand's flag set and the problems it reports.
type subcommand struct {
	fs     *flag.FlagSet
	stderr io.Writer
	flags  map[string]string // the flag that sets each component input
}

func newSubcommand(name, synopsis string, stderr io.Writer, flags map[string]string) *subcommand {
	fs := flag.NewFlagSet("envoy-infra render "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: envoy-infra render %s %s\n\n", name, synopsis)
		fs.PrintDefaults()
	}
	return &subcommand{fs: fs, stderr: stderr, flags: flags}
}

// usage reports a usage error, then the subcommand's usage, and returns its exit code.
func (s *subcommand) usage(problem string) int {
	fmt.Fprintf(s.stderr, "%s: %s\n", s.fs.Name(), problem)
	s.fs.Usage()
	return 2
}

// parse parses args and checks that each of required was given. It returns the flags given, and
// the exit code when the command must stop: 0 after --help, 2 on a usage error.
func (s *subcommand) parse(args []string, required ...string) (map[string]bool, int, bool) {
	if err := s.fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0, true
		}
		return nil, 2, true // the flag package has printed the problem and the usage
	}
	if s.fs.NArg() > 0 {
		return nil, s.usage("unexpected arguments after the flags"), true
	}
	given := map[string]bool{}
	s.fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for _, name := range required {
		if !given[name] {
			return nil, s.usage("--" + name + " is required"), true
		}
	}
	return given, 0, false
}

// refused reports a refusal of the value the flag name sets and returns its exit code.
func (s *subcommand) refused(name string, err error) int {
	fmt.Fprintf(s.stderr, "refused: --%s: %v\n", name, err)
	return 2
}

// refusedInput reports the component's refusal of one of its inputs, naming the flag that set it,
// and returns its exit code. Every refusal the component shares with this command is a
// core.InputError naming an input one of the subcommand's flags sets.
func (s *subcommand) refusedInput(err error) int {
	var refused *core.InputError
	if !errors.As(err, &refused) {
		panic(fmt.Sprintf("%s: a refusal that names no input: %v", s.fs.Name(), err))
	}
	name, ok := s.flags[refused.Input]
	if !ok {
		panic(fmt.Sprintf("%s: no flag sets the refused input %s: %v", s.fs.Name(), refused.Input, err))
	}
	return s.refused(name, err)
}

// Command runs `envoy-infra render` on args, the words after "render", and returns its exit code.
func Command(args []string, stdout, stderr io.Writer) int {
	usage := func(problem string) int {
		fmt.Fprintf(stderr, "envoy-infra render: %s\n", problem)
		fmt.Fprint(stderr, usageText)
		return 2
	}
	if len(args) == 0 {
		return usage("a subcommand is required")
	}
	switch args[0] {
	case "-h", "-help", "--help":
		fmt.Fprint(stderr, usageText)
		return 0
	case "nats-start-script":
		return natsStartScript(args[1:], stdout, stderr)
	case "webhook-listener-env":
		return webhookListenerEnv(args[1:], stdout, stderr)
	case "metric-filters":
		return metricFilters(args[1:], stdout, stderr)
	default:
		return usage("the subcommand is nats-start-script, webhook-listener-env or metric-filters")
	}
}

func natsStartScript(args []string, stdout, stderr io.Writer) int {
	s := newSubcommand("nats-start-script",
		"--authorization-file <json> [--config-header <line>]... [--server-arg <arg>]... --config-path <path>", stderr,
		map[string]string{
			"authorization": "authorization-file",
			"configHeader":  "config-header",
			"configPath":    "config-path",
		})
	var authorizationFile, configPath string
	var header, serverArgs stringList
	s.fs.StringVar(&authorizationFile, "authorization-file", "",
		"authorization: a JSON file holding the component's authorization input, keyed as the schema's aws:Authorization type")
	s.fs.Var(&header, "config-header", "configHeader: one header line of the rendered configuration; give it once per line, in order")
	s.fs.Var(&serverArgs, "server-arg", "serverArgs: one argument nats-server runs with; give it once per argument, in order")
	s.fs.StringVar(&configPath, "config-path", "", "configPath: where the script writes the configuration")
	if _, code, stop := s.parse(args, "authorization-file", "config-path"); stop {
		return code
	}

	text, err := os.ReadFile(authorizationFile)
	if err != nil {
		return s.refused("authorization-file", err)
	}
	// encoding/json matches each key to the core.Authorization field of that name, which is the
	// schema's property name of the aws:Authorization input; a key no field has is refused.
	var authorization core.Authorization
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&authorization); err != nil {
		return s.refused("authorization-file", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return s.refused("authorization-file", errors.New("holds something after its JSON value"))
	}

	script, err := rendering.NatsStartScript(authorization, header, serverArgs, configPath)
	if err != nil {
		return s.refusedInput(err)
	}
	fmt.Fprint(stdout, script)
	return 0
}

func webhookListenerEnv(args []string, stdout, stderr io.Writer) int {
	s := newSubcommand("webhook-listener-env",
		"--machine-id <id> --nats-url <url>... [--webhook <name>]... [--reviewer-app-id <id>] [--mention-trigger <t>] "+
			"--listen-host <h> --host-bridge <h> --nats-replicas <n>", stderr,
		map[string]string{
			"machineId":      "machine-id",
			"natsUrls":       "nats-url",
			"webhooks":       "webhook",
			"reviewerAppId":  "reviewer-app-id",
			"mentionTrigger": "mention-trigger",
			"listenHost":     "listen-host",
			"hostBridge":     "host-bridge",
			"natsReplicas":   "nats-replicas",
		})
	var machineID, reviewerAppID, mentionTrigger, listenHost, hostBridge string
	var natsURLs, webhooks stringList
	var natsReplicas int
	s.fs.StringVar(&machineID, "machine-id", "", "machineId: the listener's name, ENVOY_MACHINE_ID")
	s.fs.Var(&natsURLs, "nats-url", "natsUrls: one NATS server's URL; give it once per URL, in order")
	s.fs.Var(&webhooks, "webhook", "webhooks: one webhook the listener serves; this version takes none, or exactly github, then slack")
	s.fs.StringVar(&reviewerAppID, "reviewer-app-id", "", "reviewerAppId: ENVOY_REVIEWER_APP_ID, required with --webhook github and refused without it")
	s.fs.StringVar(&mentionTrigger, "mention-trigger", "", "mentionTrigger: ENVOY_GITHUB_MENTION_TRIGGER, required with --webhook github and refused without it")
	s.fs.StringVar(&listenHost, "listen-host", "", "listenHost: ENVOY_LISTEN_HOST")
	s.fs.StringVar(&hostBridge, "host-bridge", "", `hostBridge: ENVOY_HOST_BRIDGE; "" leaves it unset`)
	s.fs.IntVar(&natsReplicas, "nats-replicas", 0, "natsReplicas: ENVOY_NATS_REPLICAS")
	given, code, stop := s.parse(args, "machine-id", "listen-host", "host-bridge", "nats-replicas")
	if stop {
		return code
	}

	env := rendering.ListenerEnv{
		MachineId:    machineID,
		NatsUrls:     natsURLs,
		ListenHost:   listenHost,
		HostBridge:   hostBridge,
		NatsReplicas: natsReplicas,
		Webhooks:     webhooks,
	}
	if given["reviewer-app-id"] {
		env.ReviewerAppId = &reviewerAppID
	}
	if given["mention-trigger"] {
		env.MentionTrigger = &mentionTrigger
	}
	if err := env.Check(); err != nil {
		return s.refusedInput(err)
	}
	variables, err := env.Variables()
	if err != nil {
		return s.refusedInput(err)
	}
	// The component writes each value into its JSON, where a line break is escaped; a line of
	// this output would end there.
	for _, value := range []struct {
		flag   string
		values []string
	}{
		{"machine-id", []string{machineID}},
		{"nats-url", natsURLs},
		{"listen-host", []string{listenHost}},
		{"host-bridge", []string{hostBridge}},
		{"mention-trigger", []string{mentionTrigger}},
		{"reviewer-app-id", []string{reviewerAppID}},
	} {
		for _, v := range value.values {
			if strings.ContainsAny(v, "\r\n") {
				return s.refused(value.flag, errors.New("holds a line break, which a KEY=value line cannot carry"))
			}
		}
	}
	for _, variable := range variables {
		fmt.Fprintln(stdout, variable)
	}
	return 0
}

func metricFilters(args []string, stdout, stderr io.Writer) int {
	s := newSubcommand("metric-filters",
		"--component nats-service|webhook-listener --metric-namespace <ns> --logical-name-prefix <p> --json", stderr, nil)
	var component, namespace, prefix string
	var asJSON bool
	s.fs.StringVar(&component, "component", "", "nats-service (aws:NatsService) or webhook-listener (aws:WebhookListener)")
	s.fs.StringVar(&namespace, "metric-namespace", "", "metricNamespace: the namespace the filters' metrics are in")
	s.fs.StringVar(&prefix, "logical-name-prefix", "",
		"what the filters' logical names start with: a NatsService's metricFilterLogicalNamePrefix, a WebhookListener's own name")
	s.fs.BoolVar(&asJSON, "json", false, "print JSON, the one output format")
	if _, code, stop := s.parse(args, "component", "metric-namespace", "logical-name-prefix", "json"); stop {
		return code
	}
	if !asJSON {
		return s.usage("--json is required: JSON is the one output format")
	}

	var filters []rendering.MetricFilter
	switch component {
	case "nats-service":
		filters = rendering.NatsMetricFilters(prefix, namespace)
	case "webhook-listener":
		filters = rendering.ListenerMetricFilters(prefix, namespace)
	default:
		return s.usage("--component is nats-service or webhook-listener")
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(filters); err != nil {
		panic(fmt.Sprintf("%s: encoding the filters: %v", s.fs.Name(), err)) // a []rendering.MetricFilter always encodes
	}
	fmt.Fprint(stdout, out.String())
	return 0
}
