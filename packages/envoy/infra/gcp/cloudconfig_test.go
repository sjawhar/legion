package gcp_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
)

func TestCloudConfigMatchesTheGoldenByteForByte(t *testing.T) {
	_, text, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)
	require.Equal(t, readGolden(t, "cloud-config.golden"), text)
}

func TestCloudConfigWithoutListenerOrSessionUserMatchesTheGoldenByteForByte(t *testing.T) {
	args := exampleArgs()
	args.Listener, args.SessionUser = nil, nil
	_, text, err := gcp.CloudConfigOf(args)
	require.NoError(t, err)
	require.Equal(t, readGolden(t, "cloud-config-without-listener-or-session-user.golden"), text)
}

// cloud-init reads the user data as a cloud-config only when its first line is #cloud-config,
// and YAML reads that line as a comment, so the rest is the document itself.
func TestCloudConfigParsesBackIntoTheSameDocument(t *testing.T) {
	doc, _, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)
	golden := readGolden(t, "cloud-config.golden")
	require.True(t, strings.HasPrefix(golden, "#cloud-config\n"))

	var parsed gcp.CloudConfig
	decoder := yaml.NewDecoder(bytes.NewReader([]byte(golden)))
	decoder.KnownFields(true)
	require.NoError(t, decoder.Decode(&parsed))
	require.Equal(t, doc, parsed)
}

// The order matters: the mount point exists before the disk entry runs, which formats the disk
// only when it is proven empty and then mounts it, and the directories the containers bind are
// made on it; the key file exists, empty, before any container binds it; the units are written
// before they are loaded and started, the key fetched before tailscaled starts, the host firewall
// opened before NATS starts. The golden pins each entry's text; these are the relations between
// them.
func TestCloudConfigOrdersBootcmdFilesAndUnits(t *testing.T) {
	doc, _, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)

	mountPoint := slices.Index(doc.Bootcmd, "mkdir -p /mnt/disks/data")
	disk := indexOf(t, doc.Bootcmd, "blkid -p ")
	dirs := indexOf(t, doc.Bootcmd, "mkdir -p /mnt/disks/data/nats /mnt/disks/data/tailscale")
	require.NotEqual(t, -1, mountPoint, "the mount point is made")
	require.Less(t, mountPoint, disk, "the mount point exists before the disk is mounted on it")
	entry := doc.Bootcmd[disk]
	format := strings.Index(entry, "mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard ")
	mount := strings.Index(entry, "mount -o discard,defaults ")
	require.NotEqual(t, -1, format)
	require.Less(t, format, mount, "the disk is formatted before it is mounted")
	require.Less(t, disk, dirs, "the containers' directories are made on the mounted disk")
	indexOf(t, doc.Bootcmd, "install -m 0600 /dev/null /run/envoy/tailnet-auth-key")

	var units []string
	for _, f := range doc.WriteFiles {
		if name, ok := strings.CutPrefix(f.Path, "/etc/systemd/system/"); ok {
			units = append(units, name)
			require.Equal(t, "0644", f.Permissions, f.Path)
		}
	}
	started := []string{}
	for _, c := range doc.Runcmd[1:] {
		started = append(started, strings.Fields(c)[len(strings.Fields(c))-1])
	}
	require.Equal(t, "systemctl daemon-reload", doc.Runcmd[0], "the units are loaded before any starts")
	require.Equal(t, units, started, "runcmd starts every unit write_files writes, in the order it writes them")
	require.Less(t, slices.Index(started, "fetch-tailnet-key.service"), slices.Index(started, "tailscaled.service"))
	require.Less(t, slices.Index(started, "nats-firewall.service"), slices.Index(started, "nats.service"))
	require.Subset(t, started, []string{"fetch-tailnet-key.service", "tailscaled.service", "nats-firewall.service", "nats.service"})
	for _, c := range doc.Runcmd {
		require.NotContains(t, c, "enable", "/etc does not outlive the boot, so no unit is enabled")
	}
}

// indexOf returns the index of the first entry of list that holds text.
func indexOf(t *testing.T, list []string, text string) int {
	t.Helper()
	for i, entry := range list {
		if strings.Contains(entry, text) {
			return i
		}
	}
	t.Fatalf("no entry holds %q: %q", text, list)
	return -1
}

// fileOf returns the content of the cloud-config's file at path.
func fileOf(t *testing.T, doc gcp.CloudConfig, path string) string {
	t.Helper()
	for _, f := range doc.WriteFiles {
		if f.Path == path {
			return f.Content
		}
	}
	t.Fatalf("the cloud-config writes no %s", path)
	return ""
}

// Container-Optimized OS's host firewall accepts incoming connections for SSH alone
// (https://cloud.google.com/container-optimized-os/docs/how-to/firewall), so before NATS starts the
// VM opens the monitoring port to Google's health checks, the sources the VPC firewall rule admits,
// and the client port to the tailnet's interface, each rule only when it is not there yet. The
// tailnet's rule is the host's own, whichever iptables backend Tailscale's image writes its rules
// with.
func TestCloudConfigOpensTheHostFirewallToTheHealthChecksAndTheTailnet(t *testing.T) {
	doc, _, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)

	firewall := fileOf(t, doc, "/etc/systemd/system/nats-firewall.service")
	for _, rule := range []string{
		"INPUT -p tcp -s 130.211.0.0/22,35.191.0.0/16 --dport 8222 -j ACCEPT",
		"INPUT -i tailscale0 -p tcp --dport 4222 -j ACCEPT",
	} {
		require.Contains(t, firewall, "\nExecStart=/bin/sh -c '/sbin/iptables -w -C "+rule+" || /sbin/iptables -w -A "+rule+"'\n")
	}
	require.Equal(t, 2, strings.Count(firewall, "\nExecStart="), "the unit opens nothing else")
	nats := fileOf(t, doc, "/etc/systemd/system/nats.service")
	require.Contains(t, nats, "\nWants=network-online.target nats-firewall.service\n")
	require.Contains(t, nats, "\nAfter=docker.service network-online.target nats-firewall.service\n")
}

// The configuration file is core's rendering of the users: each of users a keyed user with a seed,
// a machine id giving the host's permission set and explicit permissions taken as they are; then
// the listener's user, named after the hub, with the listener's set for that machine and a seed;
// then the session user, with the session's set and no seed, which a client presenting no
// credential becomes.
func TestCloudConfigNatsConfigurationIsCoresRenderingOfTheUsers(t *testing.T) {
	doc, _, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)

	host := core.HostPermissions("example-host-a")
	listener := core.ListenerPermissions("example-hub")
	session := core.SessionPermissions()
	want, err := core.Authorization{Users: []core.User{
		{Name: "example-host-a", NKey: keyA, Publish: &host.Publish, Subscribe: &host.Subscribe, HasSeed: true},
		{
			Name: "example-operator", NKey: keyB, HasSeed: true,
			Publish:   &core.Rules{Allow: []string{">"}},
			Subscribe: &core.Rules{Allow: []string{">"}, Deny: []string{"$SYS.>"}},
		},
		{Name: "example-hub", NKey: keyListener, Publish: &listener.Publish, Subscribe: &listener.Subscribe, HasSeed: true},
		{Name: "example-sessions", NKey: keySessions, Publish: &session.Publish, Subscribe: &session.Subscribe},
	}, NoAuthUser: "example-sessions"}.Render([]string{"Rendered by envoy-infra. Public keys only."})
	require.NoError(t, err)
	require.Equal(t, want, fileOf(t, doc, "/etc/nats/nats-server.conf"))
}

// Without a listener or a session user the configuration holds users alone, and a client
// presenting no credential is no user at all.
func TestCloudConfigNatsConfigurationWithUsersAlone(t *testing.T) {
	args := exampleArgs()
	args.Listener, args.SessionUser = nil, nil
	doc, _, err := gcp.CloudConfigOf(args)
	require.NoError(t, err)

	conf := fileOf(t, doc, "/etc/nats/nats-server.conf")
	require.NotContains(t, conf, "no_auth_user")
	require.NotContains(t, conf, keyListener)
	require.Contains(t, conf, keyA)
}

func TestCloudConfigUnitsFollowTheHostnameTagsAndImages(t *testing.T) {
	args := exampleArgs()
	args.Hostname = "other-hub"
	args.Tags = []string{"tag:example-host", "tag:example-hub"}
	args.NatsImage = strings.Replace(natsImage, "example.invalid/nats", "example.invalid/other-nats", 1)
	args.TailscaleImage = strings.Replace(tailscaleImage, "example.invalid/tailscale", "example.invalid/other-tailscale", 1)
	doc, _, err := gcp.CloudConfigOf(args)
	require.NoError(t, err)

	require.Contains(t, fileOf(t, doc, "/etc/systemd/system/fetch-tailnet-key.service"),
		"\nExecStart=/etc/envoy/fetch-secret /run/envoy/tailnet-auth-key other-hub-tailnet-auth-key\n")
	tailscaled := fileOf(t, doc, "/etc/systemd/system/tailscaled.service")
	require.Contains(t, tailscaled, " -e TS_HOSTNAME=other-hub ")
	require.Contains(t, tailscaled, " -e TS_EXTRA_ARGS=--advertise-tags=tag:example-host,tag:example-hub "+args.TailscaleImage+"\n")
	nats := fileOf(t, doc, "/etc/systemd/system/nats.service")
	require.Contains(t, nats, ":ro "+args.NatsImage+" -c /etc/nats/nats-server.conf -js --store_dir /data -m 8222 --server_name other-hub\n")
	listener := fileOf(t, doc, "/etc/systemd/system/envoy-listener.service")
	require.Contains(t, listener, " -e ENVOY_MACHINE_ID=other-hub ",
		"the listener runs as the hub's machine, whose durable and queue its user's grant names")
	require.Contains(t, listener, "\nExecStartPre=/etc/envoy/listener-seed-env /run/envoy/listener.env "+listenerSeed+"\n")
}

// The listener listens on the VM's loopback alone, where Tailscale Serve forwards the tailnet's
// connections, reaches NATS there too, and needs no remote-NATS opt-in for it. It gets its seed
// from the env file the unit writes before each start, never from its arguments, and starts
// after NATS. Serve's configuration forwards the listener's port, sits in a directory of its own
// that the Tailscale container mounts, and is the file TS_SERVE_CONFIG names.
func TestCloudConfigRunsTheListenerBehindTailscaleServe(t *testing.T) {
	doc, _, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)

	listener := fileOf(t, doc, "/etc/systemd/system/envoy-listener.service")
	require.Contains(t, listener, " --env-file /run/envoy/listener.env ")
	require.Contains(t, listener, " -e ENVOY_LISTEN_HOST=127.0.0.1 ")
	require.Contains(t, listener, " -e NATS_URLS=nats://127.0.0.1:4222 ")
	require.NotContains(t, listener, "ENVOY_ALLOW_REMOTE_NATS")
	require.NotContains(t, listener, "NATS_NKEY_SEED", "the seed reaches the listener through the env file alone")
	require.Contains(t, listener, "\nAfter=docker.service network-online.target nats.service\n")
	require.Contains(t, listener, "\nTimeoutStartSec=600\n")
	require.Equal(t, "systemctl start envoy-listener.service", doc.Runcmd[len(doc.Runcmd)-1],
		"the listener starts after NATS")

	require.JSONEq(t, `{"TCP":{"9020":{"TCPForward":"127.0.0.1:9020"}}}`, fileOf(t, doc, "/etc/tailscale-serve/serve.json"))
	tailscaled := fileOf(t, doc, "/etc/systemd/system/tailscaled.service")
	require.Contains(t, tailscaled, " -v /etc/tailscale-serve:/etc/tailscale-serve:ro ")
	require.Contains(t, tailscaled, " -e TS_SERVE_CONFIG=/etc/tailscale-serve/serve.json ")
}

// A hub without a listener starts none and writes an empty Serve configuration. Tailscale keeps
// its state on the data disk, so TS_SERVE_CONFIG must name that empty configuration to clear a
// forward a prior listener rendered.
func TestCloudConfigWithoutAListenerClearsTailscaleServe(t *testing.T) {
	args := exampleArgs()
	args.Listener = nil
	doc, _, err := gcp.CloudConfigOf(args)
	require.NoError(t, err)

	for _, f := range doc.WriteFiles {
		require.NotContains(t, []string{
			"/etc/systemd/system/envoy-listener.service", "/etc/envoy/listener-seed-env",
		}, f.Path)
	}
	require.NotContains(t, doc.Runcmd, "systemctl start envoy-listener.service")
	require.JSONEq(t, `{}`, fileOf(t, doc, "/etc/tailscale-serve/serve.json"))
	tailscaled := fileOf(t, doc, "/etc/systemd/system/tailscaled.service")
	require.Contains(t, tailscaled, " -v /etc/tailscale-serve:/etc/tailscale-serve:ro ")
	require.Contains(t, tailscaled, " -e TS_SERVE_CONFIG=/etc/tailscale-serve/serve.json ")
}
