package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A role process's addresses are fixed when it is launched, and a daemon restarted with its key
// changed hands other ones (handedAddresses): the worker stream is fixed for the issue pod's life,
// in every role container's launcher command, and the other five for one generation's life, in the
// start command the launcher keeps to itself. So the stream is read from the pod spec, and each
// role's five are recorded on the issue Sandbox when the daemon starts a generation, one annotation
// per role, which a later daemon reads from its informer cache (evaluate).
//
// A record names each address without its credentials — its scheme, host and port, `xxxxx@` when it
// carries userinfo, never its path, query or fragment — and holds the sha256 of its whole value,
// which is what is compared. The raw values travel only in the start command, so a URL's
// credentials never land in a cluster object.

// addressesAnnotation is the Sandbox annotation that records the addresses role's running
// generation was started with.
func addressesAnnotation(role claim.Role) string {
	return "legion.dev/addresses-" + string(role)
}

// addressRecord is one role generation's recorded addresses.
type addressRecord struct {
	Generation uint64            `json:"generation"`
	Addresses  []recordedAddress `json:"addresses"`
}

// recordedAddress is one address of a record: the variable that carries it, its name as
// namedURL (or namedNATSURLs) prints it, and the sha256 of its whole value, hex-encoded.
type recordedAddress struct {
	Variable string `json:"variable"`
	Name     string `json:"name"`
	SHA256   string `json:"sha256"`
}

// environmentAddresses are handedAddresses carried in a generation of role's environment: every
// one but the stream its launcher dials.
func (r *Runtime) environmentAddresses(role claim.Role) []handedAddress {
	return slices.DeleteFunc(r.handedAddresses(role), func(a handedAddress) bool { return a.name == connectFlag })
}

// addressName is how an address handed now is named in a record and an observation's detail.
func (r *Runtime) addressName(a handedAddress) string {
	if a.name == "ENVOY_NATS_URL" {
		return namedNATSURLs(r.natsURLs)
	}
	return namedURL(a.value)
}

func addressDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// recordFor is the record of the addresses a generation of role started now is handed, encoded as
// its annotation holds it.
func (r *Runtime) recordFor(role claim.Role, generation uint64) (string, error) {
	record := addressRecord{Generation: generation}
	for _, a := range r.environmentAddresses(role) {
		record.Addresses = append(record.Addresses, recordedAddress{Variable: a.name, Name: r.addressName(a), SHA256: addressDigest(a.value)})
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("encode the address record of generation %d: %w", generation, err)
	}
	return string(encoded), nil
}

// recordAddresses records on s, the issue Sandbox as read, the addresses a generation of role
// started now is handed: a merge patch of role's one annotation, so the other roles' records stand.
// The patch carries s's uid, which the API server refuses to change on a live object, so a Sandbox
// deleted and recreated in between is never written. It runs before the generation's start command
// is sent, so a generation that runs has its record.
func (r *Runtime) recordAddresses(ctx context.Context, s *sandbox, role claim.Role, generation uint64) error {
	record, err := r.recordFor(role, generation)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid":         string(s.UID),
		"annotations": map[string]string{addressesAnnotation(role): record},
	}})
	if err != nil {
		return err
	}
	patching, cancel := call(ctx)
	defer cancel()
	if _, err := r.sandboxClient().Patch(patching, s.Name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("record the addresses of %s generation %d on sandbox %s: %w", role, generation, s.Name, err)
	}
	return nil
}

// movedAddress is one thing a role process holds that one launched now is not handed: where it
// carries it (connectFlag, one of the variables mainEnvironment sets, or agentSecretsTokenVolume),
// what it holds there and what a process launched now is handed, each named so it carries no
// credential (namedURL, enrollmentName).
type movedAddress struct{ variable, held, handed string }

// String is the one way an observation's detail names a moved address: `<variable> <held>, now
// <handed>`.
func (m movedAddress) String() string { return m.variable + " " + m.held + ", now " + m.handed }

// describeMoved is moved as an observation's detail names them, one after another.
func describeMoved(moved []movedAddress) string {
	named := make([]string, len(moved))
	for i, m := range moved {
		named[i] = m.String()
	}
	return strings.Join(named, "; ")
}

// movedEnvironment compares role's recorded generation with the addresses a generation launched now
// is handed, one movedAddress for each that moved, its held name the record's. A Sandbox with no
// record of role, or a record of another generation, has nothing to compare: a generation started
// before records existed is never read as stale for want of one. A record that cannot be read is an
// error, which proves nothing about the generation's addresses: evaluate reads it as stale, and the
// relaunch that follows writes a fresh one.
func (r *Runtime) movedEnvironment(s *sandbox, role claim.Role, generation uint64) ([]movedAddress, error) {
	raw, ok := s.Annotations[addressesAnnotation(role)]
	if !ok {
		return nil, nil
	}
	var record addressRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return nil, fmt.Errorf("the address record %s is unreadable (%v)", addressesAnnotation(role), err)
	}
	if record.Generation != generation {
		return nil, nil
	}
	var moved []movedAddress
	for _, a := range r.environmentAddresses(role) {
		i := slices.IndexFunc(record.Addresses, func(held recordedAddress) bool { return held.Variable == a.name })
		switch {
		case i < 0:
			moved = append(moved, movedAddress{a.name, "(unrecorded)", r.addressName(a)})
		case record.Addresses[i].SHA256 != addressDigest(a.value):
			moved = append(moved, movedAddress{a.name, record.Addresses[i].Name, r.addressName(a)})
		}
	}
	return moved, nil
}

// movedStream compares the stream the named role container's launcher dials with the one a pod
// created now is handed; ok is whether it moved. A container that is absent or runs no connectFlag
// (never one this runtime built) has nothing to compare. The handed value is compared as the pod
// spec carries it, escaped against the kubelet's expansion (kubeletLiteral).
func (r *Runtime) movedStream(pod *corev1.Pod, container string) (movedAddress, bool) {
	i := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == container })
	if i < 0 {
		return movedAddress{}, false
	}
	command := pod.Spec.Containers[i].Command
	flag := slices.Index(command, connectFlag)
	if flag < 0 || flag+1 >= len(command) {
		return movedAddress{}, false
	}
	if held, handed := command[flag+1], kubeletEscape(r.streamURL); held != handed {
		return movedAddress{connectFlag, namedURL(held), namedURL(handed)}, true
	}
	return movedAddress{}, false
}

// dialsStaleStream is whether any role launcher of pod dials a stream other than the one a pod
// created now is handed: a launcher never redials elsewhere, so the pod must be replaced.
func (r *Runtime) dialsStaleStream(pod *corev1.Pod) bool {
	return slices.ContainsFunc(pod.Spec.Containers, func(c corev1.Container) bool {
		_, moved := r.movedStream(pod, c.Name)
		return moved
	})
}

// movedInPod is what pod holds, for role, that a pod created now is not handed: the stream its
// launcher dials (movedStream) and the secrets broker's enrollment its next generation is started
// against (movedEnrollment). Both are fixed for the pod's life, so either moved means the pod must
// be replaced before role can run as one launched now.
func (r *Runtime) movedInPod(pod *corev1.Pod, role claim.Role) []movedAddress {
	var moved []movedAddress
	if stream, ok := r.movedStream(pod, string(role)); ok {
		moved = append(moved, stream)
	}
	if enrollment, ok := r.movedEnrollment(pod, role); ok {
		moved = append(moved, enrollment)
	}
	return moved
}

// namedURL is the log boundary for one endpoint value: an existing pod or record can hold a value a
// previous daemon accepted, or a direct Options caller supplied, even when this daemon's current
// loader no longer accepts it. It names "(unset)" for none; otherwise it constructs the name from
// exactly the scheme, host and port it accepts to show, adding xxxxx@ when the URL carries userinfo.
// Path, query and fragment never enter it. An endpoint that cannot yield a scheme and host is xxxxx
// whole, so unexpected input cannot make the name a credential reader.
func namedURL(address string) string {
	if address == "" {
		return "(unset)"
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "xxxxx"
	}
	name := u.Scheme + "://"
	if u.User != nil {
		name += "xxxxx@"
	}
	return name + u.Host
}

// namedNATSURLs names ENVOY_NATS_URL from the distinct values r.natsURLs holds, the entries
// readNatsURLs accepted, before mainEnvironment comma-joined them: each is one endpoint, so namedURL
// parses it whole. The joined value is never split, since raw commas are valid in userinfo.
func namedNATSURLs(urls []string) string {
	if len(urls) == 0 {
		return "(unset)"
	}
	named := make([]string, len(urls))
	for i, raw := range urls {
		named[i] = namedURL(raw)
	}
	return strings.Join(named, ",")
}
