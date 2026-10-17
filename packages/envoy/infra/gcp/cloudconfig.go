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
	joinedPath   = "/etc/envoy/tailnet-joined"
	// joinedMarker is written on the data disk once the hub's node has joined the tailnet: from
	// then on no boot fetches the tailnet auth key.
	joinedMarker = dataMount + "/tailnet-joined"
	keyDir       = "/run/envoy"
	keyPath      = keyDir + "/tailnet-auth-key"
	unitDir      = "/etc/systemd/system/"
	// tailnetInterface is the interface tailscaled's kernel networking (TS_USERSPACE=false) makes.
	tailnetInterface = "tailscale0"

	fetchKeyService   = "fetch-tailnet-key.service"
	tailscaledService = "tailscaled.service"
	joinedService     = "tailnet-joined.service"
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
// device, then formats and mounts the data disk with dataDiskEntry. It makes the directories the
// two containers keep their state in, and leaves the key file empty and readable by root alone, so
// that tailscaled's bind mount is always of a file.
//
// write_files holds the NATS configuration, the key fetcher and the units; runcmd loads the units
// and starts them in order. They are started, never enabled: /etc does not outlive the boot.
func (v vm) document() cloudConfig {
	files := []writeFile{
		{Path: natsConfPath, Permissions: "0644", Content: v.natsConf},
		{Path: fetchKeyPath, Permissions: "0755", Content: fetchKeyScript},
		{Path: joinedPath, Permissions: "0755", Content: joinedScript},
	}
	runcmd := []string{"systemctl daemon-reload"}
	for _, u := range v.units() {
		files = append(files, writeFile{Path: unitDir + u.name, Permissions: "0644", Content: u.text})
		runcmd = append(runcmd, "systemctl start "+u.name)
	}
	return cloudConfig{
		Bootcmd: []string{
			"test -c /dev/net/tun || modprobe tun",
			"mkdir -p " + dataMount,
			dataDiskEntry,
			"mkdir -p " + dataMount + "/nats " + dataMount + "/tailscale",
			"mkdir -p -m 0700 " + keyDir,
			"install -m 0600 /dev/null " + keyPath,
		},
		WriteFiles: files,
		Runcmd:     runcmd,
	}
}

// dataDiskEntry formats the data disk only when it is proven empty end to end: blkid finds no
// filesystem on it (exit status 2) and cmp reads the whole device, blockdev's size of it, against
// /dev/zero, stopping at the first byte that is not zero. Container-Optimized OS ships GNU cmp
// (diffutils), which takes a length over 4 GiB, as busybox's does not. A disk blkid finds a
// filesystem on is not read: it is checked with fsck.ext4 -p, as Container-Optimized OS's
// documentation does before a mount
// (https://cloud.google.com/container-optimized-os/docs/concepts/disks-and-filesystem), and mounted,
// with the options Compute Engine documents for a persistent disk
// (https://cloud.google.com/compute/docs/disks/format-mount-disk-linux), when fsck left no error it
// could not fix (exit status below 4). Every other disk is left unmounted and unwritten, among them
// a filesystem whose first MiB or primary superblock was zeroed, which still holds its files'
// blocks for mkfs.ext4's discard to destroy (a damaged superblock alone, e2fsck repairs from a
// backup). The entry then prints one line naming the device and the reason and nothing the disk
// holds, at every boot that refuses it; an unmounted disk keeps both containers down, since their
// units refuse to start without it.
const dataDiskEntry = `d=` + dataDevice + `
data_disk() {
  blkid -p "$d" >/dev/null; s=$?
  if [ "$s" -eq 2 ]; then
    size=$(blockdev --getsize64 "$d") || { why="blkid finds no filesystem on it, and blockdev --getsize64 exited $?"; return 1; }
    cmp -s -n "$size" /dev/zero "$d"; s=$?
    if [ "$s" -eq 1 ]; then why="blkid finds no filesystem on it, and it does not read as zeros to its end"; return 1; fi
    if [ "$s" -ne 0 ]; then why="blkid finds no filesystem on it, and cmp exited $s reading it"; return 1; fi
    mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard "$d" || { why="mkfs.ext4 exited $?"; return 1; }
  elif [ "$s" -ne 0 ]; then
    why="blkid -p exited $s"; return 1
  fi
  fsck.ext4 -p "$d"; s=$?
  if [ "$s" -ge 4 ]; then why="fsck.ext4 -p exited $s"; return 1; fi
  mount -o discard,defaults "$d" ` + dataMount + ` || { why="mount exited $?"; return 1; }
}
data_disk || echo "envoy-hub: data disk $d left unmounted: $why; see docs/nats-hub.md, \"Repairing the data disk\"" >&2`

// unitFile is one of the VM's systemd units: its file name under unitDir, and its text.
type unitFile struct{ name, text string }

// units are the VM's units, in the order runcmd starts them: the key is fetched before tailscaled
// starts, tailnet-joined watches tailscaled once it has, and the host firewall is opened before
// NATS starts.
func (v vm) units() []unitFile {
	return []unitFile{
		{fetchKeyService, v.fetchKeyUnit()},
		{tailscaledService, v.tailscaledUnit()},
		{joinedService, joinedUnit()},
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

// fetchKeyUnit runs the key fetcher once per boot, on a mounted data disk that holds no joined
// marker: once the node has joined, the key is never fetched again. It stays active after it
// exits, so that a later start of tailscaled does not fetch the key again.
func (v vm) fetchKeyUnit() string {
	return unit(
		"[Unit]",
		"Description=Fetch the tailnet auth key from Secret Manager",
		"Wants=network-online.target",
		"After=network-online.target",
		"ConditionPathExists=!"+joinedMarker,
		"RequiresMountsFor="+dataMount,
		"AssertPathIsMountPoint="+dataMount,
		"",
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"ExecStart="+fetchKeyPath+" "+keyPath+" "+v.keySecret,
	)
}

// joinedScript waits, by /bin/sh, until the Tailscale container reports the node Running, asking
// every 5 seconds through containerboot's socket (TS_SOCKET's default, tailscale v1.104.1
// cmd/containerboot/settings.go:125), then empties the key file, keeping the file itself, which
// tailscaled's container binds, and only once that has succeeded writes the marker it is given. A
// node that has joined logs in from its state on the data disk at every later boot (TS_AUTH_ONCE),
// so the key is on the VM no longer than the first login needs. A drop that fails writes no marker
// and exits non-zero: its unit runs the script again, and every later boot fetches and drops the
// key again until a drop succeeds. tailscaled writes its state file before it logs in (tailscale
// v1.104.1 ipn/ipnlocal/local.go:3185-3190, 4496-4526), so the file's existence does not mean the
// node has joined: hence the marker.
const joinedScript = `#!/bin/sh
# tailnet-joined MARKER KEY_FILE
set -u
marker=$1
key=$2
until docker exec tailscaled tailscale --socket=/tmp/tailscaled.sock status --json 2>/dev/null |
  jq -e '.BackendState == "Running"' >/dev/null; do
  sleep 5
done
: >"$key" && touch "$marker"
`

// joinedUnit runs the joined script once tailscaled has started, until the node has joined, on a
// mounted data disk that holds no marker yet. It is a simple service, so its start returns at once
// and runcmd goes on to NATS, and it stays active once the script exits. A run that fails, as one
// whose key drop fails does, is run again 10 seconds later.
func joinedUnit() string {
	return unit(
		"[Unit]",
		"Description=Mark this hub joined to the tailnet once Tailscale runs, and drop the tailnet auth key",
		"After="+tailscaledService,
		"ConditionPathExists=!"+joinedMarker,
		"RequiresMountsFor="+dataMount,
		"AssertPathIsMountPoint="+dataMount,
		"",
		"[Service]",
		"Type=simple",
		"RemainAfterExit=yes",
		"ExecStart="+joinedPath+" "+joinedMarker+" "+keyPath,
		"Restart=on-failure",
		"RestartSec=10",
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
