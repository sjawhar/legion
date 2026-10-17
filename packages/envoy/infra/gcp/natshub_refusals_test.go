package gcp_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/nats"
	"github.com/sjawhar/envoy/internal/testnats"
)

func TestNatsHubRefusesAMissingProviderBeforeRegistering(t *testing.T) {
	m, err := run(t, exampleArgs(), nil, false)
	require.ErrorContains(t, err, `envoy-infra:gcp:NatsHub needs a "gcp" provider in its providers option`)
	require.Empty(t, m.Types())
}

// The resolver refuses every invalid input, naming it, with no engine; the constructor's own test
// below shows a refusal stops it before anything registers.
func TestNatsHubResolverRefusesInvalidInputs(t *testing.T) {
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	seed, err := user.Seed()
	require.NoError(t, err)
	cases := []resolverRefusal{
		{
			name: "a hostname ending in a hyphen",
			edit: func(a *gcp.NatsHubArgs) { a.Hostname = "ab-" },
			err:  []string{`hostname "ab-"`, "^[a-z][a-z0-9-]{0,24}[a-z0-9]$"},
		},
		{
			name: "a one-character hostname",
			edit: func(a *gcp.NatsHubArgs) { a.Hostname = "a" },
			err:  []string{`hostname "a"`, "^[a-z][a-z0-9-]{0,24}[a-z0-9]$"},
		},
		{
			name: "a 27-character hostname",
			edit: func(a *gcp.NatsHubArgs) { a.Hostname = "a" + strings.Repeat("b", 26) },
			err:  []string{`hostname "a` + strings.Repeat("b", 26) + `"`},
		},
		{
			name: "a hostname starting with a digit",
			edit: func(a *gcp.NatsHubArgs) { a.Hostname = "1hub" },
			err:  []string{`hostname "1hub"`},
		},
		{
			name: "a hostname with an uppercase letter",
			edit: func(a *gcp.NatsHubArgs) { a.Hostname = "Example-hub" },
			err:  []string{`hostname "Example-hub"`},
		},
		{
			name: "no tags",
			edit: func(a *gcp.NatsHubArgs) { a.Tags = nil },
			err:  []string{"tags", "at least one"},
		},
		{
			name: "a tag without its prefix",
			edit: func(a *gcp.NatsHubArgs) { a.Tags = []string{"example-host"} },
			err:  []string{`tags[0] "example-host"`, "^tag:[A-Za-z][A-Za-z0-9-]*$"},
		},
		{
			name: "a tag starting with a digit",
			edit: func(a *gcp.NatsHubArgs) { a.Tags = []string{"tag:example-host", "tag:1host"} },
			err:  []string{`tags[1] "tag:1host"`},
		},
		{
			name: "a tag with a comma",
			edit: func(a *gcp.NatsHubArgs) { a.Tags = []string{"tag:a,tag:b"} },
			err:  []string{`tags[0] "tag:a,tag:b"`},
		},
		{
			name: "no zone",
			edit: func(a *gcp.NatsHubArgs) { a.Zone = "" },
			err:  []string{"zone is required"},
		},
		{
			name: "a NATS image without a digest",
			edit: func(a *gcp.NatsHubArgs) { a.NatsImage = "example.invalid/nats:2.12" },
			err:  []string{`natsImage "example.invalid/nats:2.12"`, "@sha256:"},
		},
		{
			name: "a Tailscale image without a digest",
			edit: func(a *gcp.NatsHubArgs) { a.TailscaleImage = "example.invalid/tailscale:latest" },
			err:  []string{`tailscaleImage "example.invalid/tailscale:latest"`, "@sha256:"},
		},
		{
			name: "an image whose digest is short",
			edit: func(a *gcp.NatsHubArgs) { a.NatsImage = "example.invalid/nats@sha256:0123" },
			err:  []string{`natsImage "example.invalid/nats@sha256:0123"`},
		},
		{
			name: "an image with a space",
			edit: func(a *gcp.NatsHubArgs) {
				a.NatsImage = "example.invalid/nats --privileged x" + natsImage[len("example.invalid/nats"):]
			},
			err: []string{"natsImage"},
		},
		{
			name: "an empty machine type",
			edit: func(a *gcp.NatsHubArgs) { a.MachineType = pulumi.StringRef("") },
			err:  []string{"machineType"},
		},
		{
			name: "a zero disk size",
			edit: func(a *gcp.NatsHubArgs) { a.DiskSizeGb = pulumi.IntRef(0) },
			err:  []string{"diskSizeGb 0"},
		},
		{
			name: "a pd-balanced disk one GiB past the largest whose read fits the delay",
			edit: func(a *gcp.NatsHubArgs) { a.DiskSizeGb = pulumi.IntRef(253) },
			err:  []string{"diskSizeGb 253", "pd-balanced", "100 MiB/s", "3600", "the largest diskSizeGb that fits is 252"},
		},
		{
			name: "a pd-ssd disk one GiB past the largest whose read fits the delay",
			edit: func(a *gcp.NatsHubArgs) { a.DiskType, a.DiskSizeGb = pulumi.StringRef("pd-ssd"), pulumi.IntRef(507) },
			err:  []string{"diskSizeGb 507", "pd-ssd", "200 MiB/s", "the largest diskSizeGb that fits is 506"},
		},
		{
			name: "a disk size past any delay",
			edit: func(a *gcp.NatsHubArgs) { a.DiskSizeGb = pulumi.IntRef(1 << 62) },
			err:  []string{"diskSizeGb 4611686018427387904", "the largest diskSizeGb that fits is 252"},
		},
		{
			name: "a pd-standard disk",
			edit: func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef("pd-standard") },
			err:  []string{`diskType "pd-standard"`, "0.12 MiB/s per GiB", "3600"},
		},
		{
			name: "a hyperdisk-balanced disk",
			edit: func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef("hyperdisk-balanced") },
			err:  []string{`diskType "hyperdisk-balanced"`, "E2", "account team"},
		},
		{
			name: "another disk type",
			edit: func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef("pd-extreme") },
			err:  []string{`diskType "pd-extreme"`, "pd-balanced or pd-ssd"},
		},
		{
			name: "an empty disk type",
			edit: func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef("") },
			err:  []string{"diskType is empty", "pd-balanced"},
		},
		{
			name: "no tailnet auth key",
			edit: func(a *gcp.NatsHubArgs) { a.TailnetAuthKey = nil },
			err:  []string{"tailnetAuthKey is required"},
		},
		{
			name: "a subnetwork without a network",
			edit: func(a *gcp.NatsHubArgs) { a.Subnetwork = pulumi.StringPtr("example-subnetwork") },
			err:  []string{"subnetwork", "network"},
		},
		{
			name: "no users and no listener",
			edit: func(a *gcp.NatsHubArgs) { a.Users, a.Listener = nil, nil },
			err:  []string{"users: at least one user is required, or a listener"},
		},
		{
			name: "a user naming both a machine id and permissions",
			edit: func(a *gcp.NatsHubArgs) {
				a.Users[0].Permissions = &gcp.PermissionsArgs{
					Publish: nats.RulesArgs{Allow: []string{">"}}, Subscribe: nats.RulesArgs{Allow: []string{">"}},
				}
			},
			err: []string{"users[0]: machineId and permissions are both set; name one"},
		},
		{
			name: "a user naming both machine ids",
			edit: func(a *gcp.NatsHubArgs) {
				a.Users[0].DispatchMachineID = pulumi.StringRef("example-dispatch")
			},
			err: []string{"users[0]: machineId and dispatchMachineId are both set; name one"},
		},
		{
			name: "a user naming Dispatch and explicit permissions",
			edit: func(a *gcp.NatsHubArgs) {
				a.Users[2].Permissions = &gcp.PermissionsArgs{
					Publish: nats.RulesArgs{Allow: []string{">"}}, Subscribe: nats.RulesArgs{Allow: []string{">"}},
				}
			},
			err: []string{"users[2]: dispatchMachineId and permissions are both set; name one"},
		},
		{
			name: "a user naming both a machine id and daemon",
			edit: func(a *gcp.NatsHubArgs) { a.Users[0].Daemon = true },
			err:  []string{`users[0]: machineId and daemon are both set`},
		},
		{
			name: "a user naming both Dispatch and daemon",
			edit: func(a *gcp.NatsHubArgs) { a.Users[2].Daemon = true },
			err:  []string{"users[2]: dispatchMachineId and daemon are both set; name one"},
		},
		{
			name: "a user naming both daemon and permissions",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Daemon = true },
			err:  []string{`users[1]: daemon and permissions are both set`},
		},
		{
			name: "a user naming neither a machine id nor permissions",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Permissions = nil },
			err:  []string{`users[1]: name machineId, for its host's permission set, dispatchMachineId, for Dispatch's, daemon, for the daemon's, or permissions`},
		},
		{
			name: "a user with an empty machine id",
			edit: func(a *gcp.NatsHubArgs) { a.Users[0].MachineID = pulumi.StringRef("") },
			err:  []string{`users[0].machineId is empty`},
		},
		{
			name: "a user with an empty Dispatch machine id",
			edit: func(a *gcp.NatsHubArgs) { a.Users[2].DispatchMachineID = pulumi.StringRef("") },
			err:  []string{`users[2].dispatchMachineId is empty`},
		},
		{
			name: "a user without a public key",
			edit: func(a *gcp.NatsHubArgs) { a.Users[0].NKey = "" },
			err:  []string{`users[0]: nkey is required`},
		},
		{
			name: "a user whose public key is a seed",
			edit: func(a *gcp.NatsHubArgs) { a.Users[0].NKey = string(seed) },
			err:  []string{`users[0] "example-host-a"`, "never a seed"},
		},
		{
			name: "a user with an invalid name",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Name = "Example Operator" },
			err:  []string{`users[1] "Example Operator"`, "name"},
		},
		{
			name: "two users of one name",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Name = "example-host-a" },
			err:  []string{`users[1] "example-host-a"`, "repeats users[0]"},
		},
		{
			name: "explicit permissions with an empty allow list",
			edit: func(a *gcp.NatsHubArgs) { a.Users[1].Permissions.Subscribe.Allow = nil },
			err:  []string{`users[1] "example-operator"`, "subscribe.allow"},
		},
	}
	requireResolverRefusals(t, cases)
}

// Every machine id of the hub goes through core's one rule, machineId and dispatchMachineId alike:
// the id names the durable listener-<id> and the queue group envoy-listener-<id>. The rule refuses
// an empty id, leading or trailing whitespace, and every character those names cannot carry. A
// refused printable delimiter is quoted; a control or non-space whitespace character is not. The
// seed check runs first (TestNatsHubRefusesASeedInAnyPlainInput), so no refusal echoes a seed.
func TestNatsHubRefusesAMachineIdItsDurableCannotCarry(t *testing.T) {
	kinds := []struct {
		field, where string
		set          func(*gcp.NatsHubArgs, string)
	}{
		{"machineId", "users[0]", func(a *gcp.NatsHubArgs, id string) { a.Users[0].MachineID = pulumi.StringRef(id) }},
		{"dispatchMachineId", "users[2]", func(a *gcp.NatsHubArgs, id string) { a.Users[2].DispatchMachineID = pulumi.StringRef(id) }},
	}
	const carry = ", which cannot appear in the durable name listener-<id> or the queue group envoy-listener-<id>"
	for _, kind := range kinds {
		for _, row := range []struct{ name, id, want string }{
			{"empty", "", "is empty"},
			{"only ordinary spaces", "   ", "has leading or trailing whitespace"},
			{"leading ordinary space", " example-host", "has leading or trailing whitespace"},
			{"trailing ordinary space", "example-host ", "has leading or trailing whitespace"},
			{"leading non-breaking space", "\u00a0example-host", "has leading or trailing whitespace"},
			{"a dot", "a.b", `contains '.'` + carry},
			{"a star", "a*", `contains '*'` + carry},
			{"a full wildcard", "a>", `contains '>'` + carry},
			{"a slash", "a/b", `contains '/'` + carry},
			{"a backslash", `a\b`, `contains '\\'` + carry},
			{"an interior tab", "a\tb", "contains a control character" + carry},
			{"an interior newline", "a\nb", "contains a control character" + carry},
			{"interior non-breaking space", "a\u00a0b", "contains whitespace other than a space" + carry},
		} {
			t.Run(kind.field+" with "+row.name, func(t *testing.T) {
				args := exampleArgs()
				kind.set(args, row.id)
				input := kind.where + "." + kind.field
				requireMachineIDRefusal(t, args, input, input+" "+row.want)
			})
		}
	}
	// An interior ordinary space is accepted: both names turn it into a hyphen.
	t.Run("an id with an interior ordinary space is accepted", func(t *testing.T) {
		args := exampleArgs()
		args.Users[0].MachineID = pulumi.StringRef("example host a")
		args.Users[2].DispatchMachineID = pulumi.StringRef("example dispatch host")
		require.NoError(t, gcp.ResolveNatsHub(args))
	})
}

// Two machine ids whose durable names collide would put two listeners on one durable and queue
// group, so the later id is refused, within a kind, across the two kinds, when a space and a
// hyphen hyphenate alike, and against the hub's own listener (machine example-hub). Without the
// hub's listener the hostname is nobody's durable, so a user may hold it.
func TestNatsHubRefusesMachineIdsWhoseDurablesCollide(t *testing.T) {
	_, key := testnats.User(t)
	for _, row := range []struct {
		name, input, want string
		edit              func(*gcp.NatsHubArgs)
	}{
		{"two machineIds of one durable", "users[3].machineId",
			"users[3]: machineId gives the same durable name as users[0].machineId",
			func(a *gcp.NatsHubArgs) {
				a.Users = append(a.Users, gcp.NATSUserArgs{Name: "example-host-b", NKey: key, MachineID: pulumi.StringRef("example-host-a")})
			}},
		{"a dispatchMachineId repeating a machineId", "users[2].dispatchMachineId",
			"users[2]: dispatchMachineId gives the same durable name as users[0].machineId",
			func(a *gcp.NatsHubArgs) { a.Users[2].DispatchMachineID = pulumi.StringRef("example-host-a") }},
		{"a space and a hyphen that hyphenate alike", "users[2].dispatchMachineId",
			"users[2]: dispatchMachineId gives the same durable name as users[0].machineId",
			func(a *gcp.NatsHubArgs) {
				a.Users[0].MachineID = pulumi.StringRef("a b")
				a.Users[2].DispatchMachineID = pulumi.StringRef("a-b")
			}},
		{"a machineId on the hub's own listener's durable", "users[0].machineId",
			"users[0]: machineId gives the same durable name as the hub's own listener",
			func(a *gcp.NatsHubArgs) { a.Users[0].MachineID = pulumi.StringRef("example-hub") }},
		{"a dispatchMachineId on the hub's own listener's durable", "users[2].dispatchMachineId",
			"users[2]: dispatchMachineId gives the same durable name as the hub's own listener",
			func(a *gcp.NatsHubArgs) { a.Users[2].DispatchMachineID = pulumi.StringRef("example-hub") }},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			row.edit(args)
			requireMachineIDRefusal(t, args, row.input, row.want)
		})
	}
	t.Run("the hostname is accepted without the hub's listener", func(t *testing.T) {
		args := exampleArgs()
		args.Listener = nil
		args.Users[0].MachineID = pulumi.StringRef("example-hub")
		require.NoError(t, gcp.ResolveNatsHub(args))
	})
}

// requireMachineIDRefusal resolves args, requiring exactly want as the refusal and the
// core.InputError naming input.
func requireMachineIDRefusal(t *testing.T, args *gcp.NatsHubArgs, input, want string) {
	t.Helper()
	err := gcp.ResolveNatsHub(args)
	require.EqualError(t, err, want)
	var refused *core.InputError
	require.True(t, errors.As(err, &refused), "%v is a core.InputError", err)
	require.Equal(t, input, refused.Input)
}

// A refused input stops the constructor, named as the hub and its name, before it registers
// anything beyond the test's provider.
func TestNatsHubRefusesAnInvalidInputBeforeRegistering(t *testing.T) {
	args := exampleArgs()
	args.Hostname = "ab-"
	m, err := run(t, args, nil, true)
	require.ErrorContains(t, err, `envoy-infra:gcp:NatsHub example-hub: hostname "ab-"`)
	require.Equal(t, []string{gcpProviderType}, m.Types(), "nothing registered beyond the test's provider")
}

// A seed in any input the hub itself renders, registers or quotes in a refusal is refused naming
// the input by its path, never the value: the hostname and the tags, which the hub's own refusals
// quote and the user data holds; the zone, machine type and disk type, which reach the group and
// the template; the images, which reach the user data; and a user's machine id, which the hub turns
// into that user's rules. The seed is found as bus.HoldsSeed finds it: any type, either case, whole,
// inside a longer value, split by '.', '-', '_' or whitespace, or percent-encoded. A user's name and
// rule entries are core's to refuse (TestNatsHubPassesOnCoresSeedRefusalOfUsers); a seed as a
// user's nkey is core's to refuse, unquoted (the resolver table's "a user whose public key is a
// seed"); and tailnetAuthKey, a secret Input, is checked once it resolves
// (TestConstructRefusesASeedAsTheTailnetAuthKey).
func TestNatsHubRefusesASeedInAnyPlainInput(t *testing.T) {
	seed, _ := testnats.User(t)
	inside := "example-" + seed
	lower := strings.ToLower(seed)
	digest := "@sha256:" + strings.Repeat("0", 64)
	requireSeedRefusals(t, seed, []seedRefusal{
		{"a seed as the hostname", "hostname", func(a *gcp.NatsHubArgs) { a.Hostname = seed }},
		{"a seed in a tag", "tags[0]", func(a *gcp.NatsHubArgs) { a.Tags = []string{"tag:" + seed} }},
		{"a seed inside the zone", "zone", func(a *gcp.NatsHubArgs) { a.Zone = inside }},
		{"a seed inside the machine type", "machineType", func(a *gcp.NatsHubArgs) { a.MachineType = pulumi.StringRef(inside) }},
		{"a seed inside the disk type", "diskType", func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef(inside) }},
		{"a lowercase disk type", "diskType", func(a *gcp.NatsHubArgs) { a.DiskType = pulumi.StringRef("pd-" + lower) }},
		{"a seed in the NATS image", "natsImage", func(a *gcp.NatsHubArgs) { a.NatsImage = "example.invalid/" + seed + digest }},
		{"a seed in the Tailscale image", "tailscaleImage", func(a *gcp.NatsHubArgs) {
			a.TailscaleImage = "example.invalid/" + seed + digest
		}},
		{"a seed inside a machine id", "users[0].machineId", func(a *gcp.NatsHubArgs) { a.Users[0].MachineID = pulumi.StringRef(inside) }},
		{"a seed inside a Dispatch machine id", "users[2].dispatchMachineId", func(a *gcp.NatsHubArgs) {
			a.Users[2].DispatchMachineID = pulumi.StringRef(inside)
		}},
		// A reader or a decoder takes a seed in lowercase, or split by '.', '-', '_' or whitespace, as
		// the seed.
		{"a lowercase seed as the hostname", "hostname", func(a *gcp.NatsHubArgs) { a.Hostname = lower }},
		{"a hostname split by a hyphen", "hostname", func(a *gcp.NatsHubArgs) { a.Hostname = splitSeed(lower, "-") }},
		{"a lowercase seed in a tag", "tags[0]", func(a *gcp.NatsHubArgs) { a.Tags = []string{"tag:" + lower} }},
		{"a tag split by a hyphen", "tags[0]", func(a *gcp.NatsHubArgs) { a.Tags = []string{"tag:" + splitSeed(seed, "-")} }},
		{"a lowercase zone", "zone", func(a *gcp.NatsHubArgs) { a.Zone = "us-" + lower }},
		{"a zone split by a hyphen", "zone", func(a *gcp.NatsHubArgs) { a.Zone = splitSeed(lower, "-") }},
		{"a lowercase machine type", "machineType", func(a *gcp.NatsHubArgs) { a.MachineType = pulumi.StringRef(lower) }},
		{"a machine type split by an underscore", "machineType", func(a *gcp.NatsHubArgs) {
			a.MachineType = pulumi.StringRef(splitSeed(seed, "_"))
		}},
		{"a lowercase NATS image", "natsImage", func(a *gcp.NatsHubArgs) { a.NatsImage = "example.invalid/" + lower + digest }},
		{"a NATS image split across path parts", "natsImage", func(a *gcp.NatsHubArgs) {
			a.NatsImage = "example.invalid/" + splitSeed(lower, ".") + digest
		}},
		{"a lowercase Tailscale image", "tailscaleImage", func(a *gcp.NatsHubArgs) {
			a.TailscaleImage = "example.invalid/" + lower + digest
		}},
		{"a Tailscale image split by a hyphen", "tailscaleImage", func(a *gcp.NatsHubArgs) {
			a.TailscaleImage = "example.invalid/" + splitSeed(lower, "-") + digest
		}},
		{"a lowercase machine id", "users[0].machineId", func(a *gcp.NatsHubArgs) { a.Users[0].MachineID = pulumi.StringRef(lower) }},
		{"a machine id split by a space", "users[0].machineId", func(a *gcp.NatsHubArgs) {
			a.Users[0].MachineID = pulumi.StringRef(splitSeed(seed, " "))
		}},
		{"a lowercase Dispatch machine id", "users[2].dispatchMachineId", func(a *gcp.NatsHubArgs) {
			a.Users[2].DispatchMachineID = pulumi.StringRef(lower)
		}},
		{"a Dispatch machine id split by a space", "users[2].dispatchMachineId", func(a *gcp.NatsHubArgs) {
			a.Users[2].DispatchMachineID = pulumi.StringRef(splitSeed(seed, " "))
		}},
	})
}

// core.Authorization refuses a seed in a user's name or a rule entry before any refusal of its own
// quotes it, naming the path and never the value, and the hub passes that refusal on as it is.
// The hub's own refusals of a user name it by its index alone, so a seed in the name never reaches
// one: they come before core reads the names.
func TestNatsHubPassesOnCoresSeedRefusalOfUsers(t *testing.T) {
	seed, _ := testnats.User(t)
	lower := strings.ToLower(seed)
	for _, row := range []struct {
		name string
		edit func(*gcp.NatsHubArgs)
		want string
	}{
		{"a seed as a user's name", func(a *gcp.NatsHubArgs) { a.Users[1].Name = seed },
			"users[1]: name holds an nkey seed (not echoed)"},
		{"a lowercase seed as a user's name", func(a *gcp.NatsHubArgs) { a.Users[1].Name = lower },
			"users[1]: name holds an nkey seed (not echoed)"},
		{"a user's name split by a hyphen", func(a *gcp.NatsHubArgs) { a.Users[1].Name = splitSeed(lower, "-") },
			"users[1]: name holds an nkey seed (not echoed)"},
		{"a seed in a publish allow entry", func(a *gcp.NatsHubArgs) {
			a.Users[1].Permissions.Publish.Allow = []string{"notifications." + seed}
		}, `users[1] "example-operator": publish.allow[0] holds an nkey seed (not echoed)`},
		{"a seed percent-encoded in a subscribe deny entry", func(a *gcp.NatsHubArgs) {
			a.Users[1].Permissions.Subscribe.Deny = []string{percentEncoded(seed)}
		}, `users[1] "example-operator": subscribe.deny[0] holds an nkey seed (not echoed)`},
		{"a lowercase seed in a publish deny entry", func(a *gcp.NatsHubArgs) {
			a.Users[1].Permissions.Publish.Deny = []string{"notifications." + lower}
		}, `users[1] "example-operator": publish.deny[0] holds an nkey seed (not echoed)`},
		{"a subscribe allow entry split across subject tokens", func(a *gcp.NatsHubArgs) {
			a.Users[1].Permissions.Subscribe.Allow = []string{"notifications." + splitSeed(seed, ".")}
		}, `users[1] "example-operator": subscribe.allow[0] holds an nkey seed (not echoed)`},
		// The hub's own refusals of a user come first, and name it by its index alone.
		{"a seed as the name of a user with no nkey", func(a *gcp.NatsHubArgs) {
			a.Users[1].Name, a.Users[1].NKey = seed, ""
		}, "users[1]: nkey is required"},
		{"a seed as the name of a user naming both a machine id and permissions", func(a *gcp.NatsHubArgs) {
			a.Users[1].Name, a.Users[1].MachineID = seed, pulumi.StringRef("example-host-b")
		}, "users[1]: machineId and permissions are both set; name one"},
		{"a seed as the name of a user naming neither", func(a *gcp.NatsHubArgs) {
			a.Users[1].Name, a.Users[1].Permissions = lower, nil
		}, "users[1]: name machineId, for its host's permission set, dispatchMachineId, for Dispatch's, daemon, for the daemon's, or permissions"},
		{"a seed as the name of a user with an empty machine id", func(a *gcp.NatsHubArgs) {
			a.Users[0].Name, a.Users[0].MachineID = seed, pulumi.StringRef("")
		}, "users[0].machineId is empty"},
		{"a seed as the session user's name", func(a *gcp.NatsHubArgs) { a.SessionUser.Name = lower },
			"sessionUser: name holds an nkey seed (not echoed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			row.edit(args)
			err := gcp.ResolveNatsHub(args)
			require.EqualError(t, err, row.want)
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
		})
	}
}

// network and subnetwork may come from another resource, so the hub checks them when they are
// known, as the firewall's and the template's inputs resolve: a seed in either fails the run
// naming it, and no registration holds it.
func TestNatsHubRefusesASeedInItsNetworkWhenItResolves(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct {
		name, input string
		edit        func(*gcp.NatsHubArgs)
	}{
		{"a seed in the network", "network", func(a *gcp.NatsHubArgs) {
			a.Network = pulumi.String("projects/example-project/global/networks/" + seed)
		}},
		{"a seed in the subnetwork", "subnetwork", func(a *gcp.NatsHubArgs) {
			a.Network = pulumi.String("example-network")
			a.Subnetwork = pulumi.String("example-" + seed)
		}},
		{"a lowercase network", "network", func(a *gcp.NatsHubArgs) {
			a.Network = pulumi.String("projects/example-project/global/networks/" + strings.ToLower(seed))
		}},
		{"a subnetwork split by a hyphen", "subnetwork", func(a *gcp.NatsHubArgs) {
			a.Network = pulumi.String("example-network")
			a.Subnetwork = pulumi.String(splitSeed(strings.ToLower(seed), "-"))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			row.edit(args)
			m, err := run(t, args, nil, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:NatsHub example-hub: "+row.input+
				": an nkey user seed, which is never an input of the hub (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			for _, r := range m.Registrations() {
				for _, s := range plainStrings(resource.NewProperty(r.Inputs)) {
					require.NotContains(t, strings.ToUpper(s), seed, "%s %s holds the seed", r.Type, r.Name)
				}
			}
		})
	}
}

// NewNatsHub quotes the name in every refusal (its "%s %s: %w" wrapper) and registers the
// component under it, so a seed there is refused before anything registers, as host:Listener and
// the AWS components refuse one in their own names.
func TestNatsHubRefusesASeedInTheComponentName(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct {
		name string
		hub  string
	}{
		{"a whole seed", seed},
		{"a seed inside a longer name", "hub-" + seed},
		{"a lowercase seed", strings.ToLower(seed)},
		{"a seed split by a hyphen", splitSeed(seed, "-")},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runNamed(t, row.hub, exampleArgs(), nil, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:NatsHub: the component's name holds an nkey seed (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			require.Equal(t, []string{gcpProviderType}, m.Types(), "nothing registered beyond the test's provider")
		})
	}
}

// percentEncoded spells every byte of text as %XX.
func percentEncoded(text string) string {
	var b strings.Builder
	for i := range len(text) {
		fmt.Fprintf(&b, "%%%02X", text[i])
	}
	return b.String()
}

// splitSeed is seed with sep between its halves.
func splitSeed(seed, sep string) string { return seed[:len(seed)/2] + sep + seed[len(seed)/2:] }

// resolverRefusal is one invalid input: edit makes it of exampleArgs, and the resolver's refusal
// contains each of err.
type resolverRefusal struct {
	name string
	edit func(*gcp.NatsHubArgs)
	err  []string
}

// requireResolverRefusals runs each case through the resolver, which must refuse it naming each
// of its err and never quote the tailnet auth key.
func requireResolverRefusals(t *testing.T, cases []resolverRefusal) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := exampleArgs()
			c.edit(args)
			err := gcp.ResolveNatsHub(args)
			require.Error(t, err)
			for _, want := range c.err {
				require.ErrorContains(t, err, want)
			}
			require.NotContains(t, err.Error(), authKey)
		})
	}
}

// seedRefusal is one plain input holding seed: edit puts it there, and input is the path the
// refusal names.
type seedRefusal struct {
	name, input string
	edit        func(*gcp.NatsHubArgs)
}

// requireSeedRefusals runs each row through the resolver, which must refuse it naming the input
// by its path and never quoting the seed.
func requireSeedRefusals(t *testing.T, seed string, rows []seedRefusal) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			row.edit(args)
			err := gcp.ResolveNatsHub(args)
			require.EqualError(t, err, row.input+": an nkey user seed, which is never an input of the hub (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
		})
	}
}
