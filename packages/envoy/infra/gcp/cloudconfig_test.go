package gcp_test

import (
	"bytes"
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

// The order matters: the disk is formatted only when it holds no filesystem, then mounted, and
// the directories the containers bind are made on it; the key file exists, empty, before any
// container binds it; the units are written before they are loaded and started, the key fetched
// before tailscaled starts, the host firewall opened before NATS starts.
func TestCloudConfigOrdersBootcmdFilesAndUnits(t *testing.T) {
	doc, _, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)

	require.Equal(t, []string{
		"test -c /dev/net/tun || modprobe tun",
		"blkid -p /dev/disk/by-id/google-data >/dev/null || [ $? -ne 2 ] || " +
			"mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard /dev/disk/by-id/google-data",
		"mkdir -p /mnt/disks/data",
		"mount -o discard,defaults /dev/disk/by-id/google-data /mnt/disks/data",
		"mkdir -p /mnt/disks/data/nats /mnt/disks/data/tailscale",
		"mkdir -p -m 0700 /run/envoy",
		"install -m 0600 /dev/null /run/envoy/tailnet-auth-key",
	}, doc.Bootcmd)

	var files []string
	for _, f := range doc.WriteFiles {
		files = append(files, f.Path+" "+f.Permissions)
	}
	require.Equal(t, []string{
		"/etc/nats/nats-server.conf 0644",
		"/etc/envoy/fetch-tailnet-key 0755",
		"/etc/systemd/system/fetch-tailnet-key.service 0644",
		"/etc/systemd/system/tailscaled.service 0644",
		"/etc/systemd/system/nats-firewall.service 0644",
		"/etc/systemd/system/nats.service 0644",
	}, files)

	require.Equal(t, []string{
		"systemctl daemon-reload",
		"systemctl start fetch-tailnet-key.service",
		"systemctl start tailscaled.service",
		"systemctl start nats-firewall.service",
		"systemctl start nats.service",
	}, doc.Runcmd)
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
// (https://cloud.google.com/container-optimized-os/docs/how-to/firewall), so the VM opens the
// monitoring port to Google's health checks, the sources the VPC firewall rule admits, before NATS
// starts, adding the rule only when it is not there yet. The tailnet needs no rule: tailscaled
// accepts all it receives on its own interface (tailscale v1.104.1
// util/linuxfw/iptables_runner.go:229-233).
func TestCloudConfigOpensTheHostFirewallToTheHealthChecksAlone(t *testing.T) {
	doc, _, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)

	require.Equal(t, strings.Join([]string{
		"[Unit]",
		"Description=Open the host firewall to Google's health checks on NATS's monitoring port",
		"",
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"ExecStart=/bin/sh -c '" +
			"/sbin/iptables -w -C INPUT -p tcp -s 130.211.0.0/22,35.191.0.0/16 --dport 8222 -j ACCEPT || " +
			"/sbin/iptables -w -A INPUT -p tcp -s 130.211.0.0/22,35.191.0.0/16 --dport 8222 -j ACCEPT'",
	}, "\n")+"\n", fileOf(t, doc, "/etc/systemd/system/nats-firewall.service"))
	nats := fileOf(t, doc, "/etc/systemd/system/nats.service")
	require.Contains(t, nats, "\nWants=network-online.target nats-firewall.service\n")
	require.Contains(t, nats, "\nAfter=docker.service network-online.target nats-firewall.service\n")
}

// The configuration file is core's rendering of the users, each a keyed user with a seed: a
// machine id gives the host's permission set, explicit permissions are taken as they are.
func TestCloudConfigNatsConfigurationIsCoresRenderingOfTheUsers(t *testing.T) {
	doc, _, err := gcp.CloudConfigOf(exampleArgs())
	require.NoError(t, err)

	host := core.HostPermissions("example-host-a")
	want, err := core.Authorization{Users: []core.User{
		{Name: "example-host-a", NKey: keyA, Publish: &host.Publish, Subscribe: &host.Subscribe, HasSeed: true},
		{
			Name: "example-operator", NKey: keyB, HasSeed: true,
			Publish:   &core.Rules{Allow: []string{">"}},
			Subscribe: &core.Rules{Allow: []string{">"}, Deny: []string{"$SYS.>"}},
		},
	}}.Render([]string{"Rendered by envoy-infra. Public keys only."})
	require.NoError(t, err)
	require.Equal(t, want, fileOf(t, doc, "/etc/nats/nats-server.conf"))
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
		"\nExecStart=/etc/envoy/fetch-tailnet-key /run/envoy/tailnet-auth-key other-hub-tailnet-auth-key\n")
	tailscaled := fileOf(t, doc, "/etc/systemd/system/tailscaled.service")
	require.Contains(t, tailscaled, " -e TS_HOSTNAME=other-hub ")
	require.Contains(t, tailscaled, " -e TS_EXTRA_ARGS=--advertise-tags=tag:example-host,tag:example-hub "+args.TailscaleImage+"\n")
	nats := fileOf(t, doc, "/etc/systemd/system/nats.service")
	require.Contains(t, nats, ":ro "+args.NatsImage+" -c /etc/nats/nats-server.conf -js --store_dir /data -m 8222 --server_name other-hub\n")
}
