package gcp

import (
	"bytes"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Where the VM keeps what its units read. Container-Optimized OS mounts / read-only; /etc and
// /mnt/disks are writable, executable and stateless, and so is cloud-init's own directory, so
// cloud-init runs every module again at every boot and writes these files anew
// (https://cloud.google.com/container-optimized-os/docs/concepts/disks-and-filesystem).
const (
	dataDevice   = "/dev/disk/by-id/google-" + dataDeviceName
	dataMount    = "/mnt/disks/data"
	natsConfPath = "/etc/nats/nats-server.conf"
	fetchKeyPath = "/etc/envoy/fetch-tailnet-key"
	keyDir       = "/run/envoy"
	keyPath      = keyDir + "/tailnet-auth-key"
	unitDir      = "/etc/systemd/system/"
	// tailnetInterface is the interface tailscaled's kernel networking (TS_USERSPACE=false) makes.
	tailnetInterface = "tailscale0"

	fetchKeyService   = "fetch-tailnet-key.service"
	tailscaledService = "tailscaled.service"
	firewallService   = "nats-firewall.service"
	natsService       = "nats.service"
)

// vm is what the hub VM's cloud-config is built from. Every field comes from a plain input, so
// the cloud-config is known at preview, and none holds a secret: the VM fetches the tailnet auth
// key at boot.
type vm struct {
	hostname       string
	keySecret      string // the secret id of the tailnet auth key, which the VM fetches at boot
	tags           []string
	natsImage      string
	tailscaleImage string
	natsConf       string // the rendered authorization, public keys only
}

// cloudConfig is the document cloud-init reads from the VM's user data.
type cloudConfig struct {
	Bootcmd    []string    `yaml:"bootcmd"`
	WriteFiles []writeFile `yaml:"write_files"`
	Runcmd     []string    `yaml:"runcmd"`
}

// writeFile is one entry of write_files.
type writeFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
}

// document returns the VM's cloud-config.
//
// bootcmd runs first at every boot. It loads the tun module unless the kernel already has the
// device. It formats the data disk only when blkid finds nothing on it, which it reports as exit
// status 2: a disk blkid cannot read some other way is left as it is, and its mount fails. It
// mounts the disk with the options Compute Engine documents for a persistent disk
// (https://cloud.google.com/compute/docs/disks/format-mount-disk-linux), makes the directories the
// two containers keep their state in, and leaves the key file empty and readable by root alone, so
// that tailscaled's bind mount is always of a file.
//
// write_files holds the NATS configuration, the key fetcher and the units; runcmd loads the units
// and starts them in order. They are started, never enabled: /etc does not outlive the boot.
func (v vm) document() cloudConfig {
	files := []writeFile{
		{Path: natsConfPath, Permissions: "0644", Content: v.natsConf},
		{Path: fetchKeyPath, Permissions: "0755", Content: fetchKeyScript},
	}
	runcmd := []string{"systemctl daemon-reload"}
	for _, u := range v.units() {
		files = append(files, writeFile{Path: unitDir + u.name, Permissions: "0644", Content: u.text})
		runcmd = append(runcmd, "systemctl start "+u.name)
	}
	return cloudConfig{
		Bootcmd: []string{
			"test -c /dev/net/tun || modprobe tun",
			"blkid -p " + dataDevice + " >/dev/null || [ $? -ne 2 ] || " +
				"mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard " + dataDevice,
			"mkdir -p " + dataMount,
			"mount -o discard,defaults " + dataDevice + " " + dataMount,
			"mkdir -p " + dataMount + "/nats " + dataMount + "/tailscale",
			"mkdir -p -m 0700 " + keyDir,
			"install -m 0600 /dev/null " + keyPath,
		},
		WriteFiles: files,
		Runcmd:     runcmd,
	}
}

// unitFile is one of the VM's systemd units: its file name under unitDir, and its text.
type unitFile struct{ name, text string }

// units are the VM's units, in the order runcmd starts them: the key is fetched before tailscaled
// starts, and the host firewall opened before NATS starts.
func (v vm) units() []unitFile {
	return []unitFile{
		{fetchKeyService, v.fetchKeyUnit()},
		{tailscaledService, v.tailscaledUnit()},
		{firewallService, firewallUnit()},
		{natsService, v.natsUnit()},
	}
}

// userData returns the document as the VM's user data: YAML after the #cloud-config line that
// marks it as a cloud-config.
func (v vm) userData() (string, error) {
	var b bytes.Buffer
	b.WriteString("#cloud-config\n")
	encoder := yaml.NewEncoder(&b)
	encoder.SetIndent(2)
	if err := encoder.Encode(v.document()); err != nil {
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// fetchKeyScript is the key fetcher, run by /bin/sh with curl, jq, base64 and coreutils, which
// Container-Optimized OS ships (cos-tools/<build>/lakitu/cos-package-info.json for M125 and M133).
// It retries itself, rather than leaving that to systemd, so that its unit's start job ends only
// once the key is written or every attempt failed, and tailscaled, ordered after it, never starts
// on the empty key file between two attempts.
const fetchKeyScript = `#!/bin/sh
# fetch-tailnet-key OUT SECRET writes the latest version of the Secret Manager secret SECRET,
# in this VM's project, to the file OUT, readable by root alone. It authenticates as the VM's
# service account, with a token from the metadata server, and tries six times, ten seconds
# apart: a new grant on the secret can take a while to reach Secret Manager.
# GCE_METADATA_HOST and SECRET_MANAGER_ENDPOINT replace the metadata server's host and Secret
# Manager's base URL. Nothing of the key or the token is printed or passed in a command's
# arguments.
set -u
out=$1
secret=$2
metadata=http://${GCE_METADATA_HOST:-metadata.google.internal}/computeMetadata/v1
endpoint=${SECRET_MANAGER_ENDPOINT:-https://secretmanager.googleapis.com}

fetch() {
  project=$(curl -fsS --max-time 30 -H 'Metadata-Flavor: Google' "$metadata/project/project-id") || return 1
  token=$(curl -fsS --max-time 30 -H 'Metadata-Flavor: Google' "$metadata/instance/service-accounts/default/token") || return 1
  token=$(printf '%s' "$token" | jq -er .access_token) || return 1
  response=$(printf 'header = "Authorization: Bearer %s"\n' "$token" |
    curl -fsS --max-time 30 --config - "$endpoint/v1/projects/$project/secrets/$secret/versions/latest:access") || return 1
  data=$(printf '%s' "$response" | jq -er .payload.data) || return 1
  tmp=$(mktemp "$out.XXXXXX") || return 1
  if printf '%s' "$data" | base64 -d >"$tmp" && [ -s "$tmp" ] && mv -f "$tmp" "$out"; then
    return 0
  fi
  rm -f "$tmp"
  echo "fetch-tailnet-key: the latest version of $secret holds no key" >&2
  return 1
}

attempt=1
until fetch; do
  if [ "$attempt" -ge 6 ]; then
    echo "fetch-tailnet-key: no key after $attempt attempts" >&2
    exit 1
  fi
  attempt=$((attempt + 1))
  sleep 10
done
`

// fetchKeyUnit runs the key fetcher once per boot. It stays active after it exits, so that a
// later start of tailscaled does not fetch the key again.
func (v vm) fetchKeyUnit() string {
	return unit(
		"[Unit]",
		"Description=Fetch the tailnet auth key from Secret Manager",
		"Wants=network-online.target",
		"After=network-online.target",
		"",
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"ExecStart="+fetchKeyPath+" "+keyPath+" "+v.keySecret,
	)
}

// tailscaledUnit runs Tailscale's container with its state on the data disk. It wants the key
// fetched, and starts after the fetch ends, but does not require it: a node whose tailnet state
// is on the disk never reads the key (TS_AUTH_ONCE), so a failed fetch must not keep it down.
// containerboot hands the key file's path to the tailscale CLI, which reads it (tailscale v1.104.1
// cmd/containerboot/tailscaled.go:120-121).
func (v vm) tailscaledUnit() string {
	return containerUnit("Tailscale, which joins this hub to the tailnet", fetchKeyService, "tailscaled", []string{
		"--cap-add", "NET_ADMIN", "--device", "/dev/net/tun",
		"-v", dataMount + "/tailscale:/var/lib/tailscale",
		"-v", keyPath + ":" + keyPath + ":ro",
		"-e", "TS_AUTHKEY=file:" + keyPath,
		"-e", "TS_AUTH_ONCE=true",
		"-e", "TS_STATE_DIR=/var/lib/tailscale",
		"-e", "TS_USERSPACE=false",
		"-e", "TS_HOSTNAME=" + v.hostname,
		"-e", "TS_EXTRA_ARGS=--advertise-tags=" + strings.Join(v.tags, ","),
		v.tailscaleImage,
	})
}

// firewallUnit opens Container-Optimized OS's host firewall, which accepts incoming connections for
// SSH alone (https://cloud.google.com/container-optimized-os/docs/how-to/firewall), to Google's
// health checks on the monitoring port, the sources the VPC firewall rule admits, and to the
// tailnet's interface on the client port. Each rule is added only when it is not there yet. The
// tailnet's rule is needed although tailscaled writes an accept rule for its own interface: the
// image writes that rule with the iptables backend it picks, and when the host filters with
// another, the host's drop policy drops the packet whatever that rule accepts. Both rules here
// are added with the host's own /sbin/iptables.
func firewallUnit() string {
	healthChecks := "INPUT -p tcp -s " + strings.Join(healthCheckRanges, ",") + " --dport " + strconv.Itoa(monitorPort) + " -j ACCEPT"
	tailnet := "INPUT -i " + tailnetInterface + " -p tcp --dport " + strconv.Itoa(natsPort) + " -j ACCEPT"
	return unit(
		"[Unit]",
		"Description=Open the host firewall to Google's health checks on NATS's monitoring port, and to the tailnet on its client port",
		"",
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"ExecStart=/bin/sh -c '/sbin/iptables -w -C "+healthChecks+" || /sbin/iptables -w -A "+healthChecks+"'",
		"ExecStart=/bin/sh -c '/sbin/iptables -w -C "+tailnet+" || /sbin/iptables -w -A "+tailnet+"'",
	)
}

// natsUnit runs nats-server with JetStream's store on the data disk, once the host firewall lets
// the health checks in. The configuration file holds the authorization alone; the server's other
// options are its arguments.
func (v vm) natsUnit() string {
	return containerUnit("NATS with JetStream", firewallService, "nats", []string{
		"-v", dataMount + "/nats:/data",
		"-v", natsConfPath + ":" + natsConfPath + ":ro",
		v.natsImage,
		"-c", natsConfPath, "-js", "--store_dir", "/data", "-m", strconv.Itoa(monitorPort), "--server_name", v.hostname,
	}, "ExecStop=/usr/bin/docker stop -t 30 nats")
}

// containerUnit is a unit that runs one container, name, on the host's network, with run its
// docker run arguments after --name and --network, and service any [Service] lines between its
// ExecStart and its restart policy. It requires docker and wants, and starts after, the network and
// after. It refuses to start unless the data disk is mounted, rather than keep its container's
// state on the tmpfs beneath, and is restarted 10 seconds after its container exits.
func containerUnit(description, after, name string, run []string, service ...string) string {
	exec := append([]string{"/usr/bin/docker", "run", "--rm", "--name", name, "--network", "host"}, run...)
	lines := []string{
		"[Unit]",
		"Description=" + description,
		"Requires=docker.service",
		"Wants=network-online.target " + after,
		"After=docker.service network-online.target " + after,
		"RequiresMountsFor=" + dataMount,
		"AssertPathIsMountPoint=" + dataMount,
		"",
		"[Service]",
		"ExecStart=" + strings.Join(exec, " "),
	}
	lines = append(lines, service...)
	return unit(append(lines, "Restart=always", "RestartSec=10")...)
}

// unit joins a unit file's lines, each ending in a newline.
func unit(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}
