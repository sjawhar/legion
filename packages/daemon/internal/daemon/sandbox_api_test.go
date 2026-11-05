package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// sandboxNamespace is the namespace every object of these tests' cluster lives in, the one
// runtime.kubernetes.namespace they configure.
const sandboxNamespace = "legion"

// issueSandboxAPI is the Kubernetes API this package's tests talk to: the one stand-in the
// daemon's issue closes, its carried-child cleanup, its kind-of-pod tests and the idle release
// and wake rig all reach the runtime through, because a runtime built by the daemon's own boot
// takes a *rest.Config and nothing else. It is the fake of
// packages/daemon/internal/runtime/sandbox/rig_test.go (its `rig` type, its `control` loop and
// its `createPod`) rebuilt over HTTP rather than over client-go's object trackers.
//
// The controller stand-in is opt-in: a test that calls run gets pods, and one that does not —
// every close test — sees a Sandbox's operatingMode as the daemon's request to the real
// controller and no pod at all. Each Sandbox stands for the disk of the pod it would have, since
// an issue pod's disk is its own generic ephemeral volume whose claim the pod owns: only a
// foreground delete, which garbage collection carries through the pod to the claim, takes it.
//
// What it serves is the subset the runtime uses: the Sandboxes, pods, Services and Secrets of one
// namespace, each resource listable, watchable (with client-go's watch-list semantics: the initial
// ADDED events and the `k8s.io/initial-events-end` bookmark), readable, writable and deletable.
// Every PersistentVolumeClaim request is refused, as the restricted daemon identity's is.
//
// The controller stand-in reconciles every Sandbox a few times a second, as the real one does: a
// Running Sandbox with no pod gets the pod its podTemplate describes, Ready, with the owner
// reference and labels the controller gives it; a Suspended Sandbox's pod is deleted; a Sandbox's
// Service is made when it asks for one and removed with it. Each pod it reports Ready starts the
// pod's processes through onPodReady — in this rig, a real `legion launcher` per role.
type issueSandboxAPI struct {
	// url is the server a test served the API on (serve), empty until one did.
	url     string
	mu      sync.Mutex
	rv      int64
	objects map[string]map[string]map[string]any
	watches map[string][]chan kubeEvent
	// unexpected is every request the API did not know, which the rig fails on: a runtime asking
	// for something this stand-in does not serve is a hole in the stand-in, never a pass.
	unexpected  []string
	pvcRequests int
	uids        int
	// disks are the disks of the Sandboxes' pods, by Sandbox name, while they exist: an issue
	// pod's disk is its own generic ephemeral volume, whose claim `<pod>-issue` the pod owns, so
	// a Sandbox's arrival stands for its pod's disk and only the Sandbox's foreground delete,
	// which garbage collection carries through the pod to the claim, takes it. deletes are the
	// options each Sandbox delete was asked with, and created every Sandbox a test's daemon made.
	disks   map[string]bool
	deletes map[string]metav1.DeleteOptions
	created []string
	// onPodReady runs, off the API's lock, for every pod the controller created and reported
	// Ready, with the pod's own uid and each of its roles' launcher tokens; onPodGone for every
	// pod it deleted. Both are the rig's.
	onPodReady func(pod, uid string, roles map[string]string)
	onPodGone  func(pod string)
}

// kubeEvent is one watch event: its type and the object it carries.
type kubeEvent struct {
	Type   string         `json:"type"`
	Object map[string]any `json:"object"`
}

// kubeResource is the plural resource name of one of the four kinds the API serves, and the kind
// and apiVersion its list and its bookmark carry.
type kubeResource struct{ resource, kind, apiVersion string }

var kubeResources = []kubeResource{
	{"sandboxes", "Sandbox", "agents.x-k8s.io/v1beta1"},
	{"pods", "Pod", "v1"},
	{"services", "Service", "v1"},
	{"secrets", "Secret", "v1"},
}

func kubeResourceOf(name string) (kubeResource, bool) {
	for _, r := range kubeResources {
		if r.resource == name {
			return r, true
		}
	}
	return kubeResource{}, false
}

// newIssueSandboxAPI is the API holding sandboxes, each with the disk of the pod it stands for.
// It serves nothing of itself: a caller mounts it on a server of its own, or calls serve.
func newIssueSandboxAPI(sandboxes ...map[string]any) *issueSandboxAPI {
	a := &issueSandboxAPI{objects: map[string]map[string]map[string]any{},
		watches: map[string][]chan kubeEvent{}, disks: map[string]bool{}, deletes: map[string]metav1.DeleteOptions{}}
	for _, r := range kubeResources {
		a.objects[r.resource] = map[string]map[string]any{}
	}
	for _, object := range sandboxes {
		name := object["metadata"].(map[string]any)["name"].(string)
		a.objects["sandboxes"][name], a.disks[name] = object, true
	}
	return a
}

// serve puts the API on a server of its own, closed with the test, and answers its URL.
func (a *issueSandboxAPI) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	a.url = server.URL
	return a.url
}

// sandboxRESTConfig is how a test's runtime reaches the API served at host. The stand-in decodes
// and encodes JSON alone, and client-go's typed clients prefer protobuf for the built-in group
// unless the configuration names a content type (rest.Request.UseProtobufAsDefaultIfPreferred),
// so it names JSON for both directions: without it a Secret write arrives as protobuf and the
// stand-in answers 400.
func sandboxRESTConfig(host string) *rest.Config {
	return &rest.Config{Host: host, ContentConfig: rest.ContentConfig{
		ContentType: "application/json", AcceptContentTypes: "application/json",
	}}
}

// issueSandbox is a Running issue Sandbox of tree and issue as the API holds it, owning no volume
// claim template: its pod's disk is the pod's own.
func issueSandbox(name, uid, tree, issue string) map[string]any {
	return map[string]any{
		"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
		"metadata": map[string]any{"name": name, "namespace": sandboxNamespace, "uid": uid, "resourceVersion": "1", "generation": float64(1),
			"labels": map[string]any{"legion.dev/project": "legion", "legion.dev/tree": tree, "legion.dev/issue": issue}},
		"spec": map[string]any{"operatingMode": "Running"},
	}
}

// run reconciles every Sandbox until t ends: the controller stand-in.
func (a *issueSandboxAPI) run(t *testing.T) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				a.reconcile()
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

// reconcile is one pass of the controller stand-in.
func (a *issueSandboxAPI) reconcile() {
	type work struct {
		pod   string
		uid   string
		roles map[string]string
		gone  bool
	}
	var todo []work
	a.mu.Lock()
	for name, object := range a.objects["sandboxes"] {
		spec, _ := object["spec"].(map[string]any)
		mode, _ := spec["operatingMode"].(string)
		if mode == "" {
			mode = "Running"
		}
		_, hasPod := a.objects["pods"][name]
		switch {
		case mode == "Running" && !hasPod:
			uid := a.createPodLocked(name, object)
			todo = append(todo, work{pod: name, uid: uid, roles: a.launcherTokensLocked(name)})
		case mode != "Running" && hasPod:
			a.deleteLocked("pods", name)
			todo = append(todo, work{pod: name, gone: true})
		}
		if service, _ := spec["service"].(bool); service {
			if _, made := a.objects["services"][name]; !made {
				uid, _ := object["metadata"].(map[string]any)["uid"].(string)
				a.putLocked("services", name, controllerService(name, uid))
			}
		}
	}
	for name := range a.objects["services"] {
		if _, live := a.objects["sandboxes"][name]; !live {
			a.deleteLocked("services", name)
		}
	}
	for name := range a.objects["pods"] {
		if _, live := a.objects["sandboxes"][name]; !live {
			a.deleteLocked("pods", name)
			todo = append(todo, work{pod: name, gone: true})
		}
	}
	ready := a.onPodReady
	gone := a.onPodGone
	a.mu.Unlock()
	for _, w := range todo {
		switch {
		case w.gone && gone != nil:
			gone(w.pod)
		case !w.gone && ready != nil:
			ready(w.pod, w.uid, w.roles)
		}
	}
}

// launcherTokensLocked is every role of the pod with the launcher token the daemon wrote into its
// role Secret: the pod's own view of <roles-dir>/<role>/launcher/LAUNCHER_TOKEN.
func (a *issueSandboxAPI) launcherTokensLocked(pod string) map[string]string {
	roles := map[string]string{}
	for name, object := range a.objects["secrets"] {
		role, ok := strings.CutPrefix(name, pod+"-")
		if !ok {
			continue
		}
		// A role's Secret is named <sandbox>-<role>-boot (runtime/sandbox's roleSecretName).
		role, ok = strings.CutSuffix(role, "-boot")
		if !ok {
			continue
		}
		data, _ := object["data"].(map[string]any)
		encoded, _ := data["LAUNCHER_TOKEN"].(string)
		if encoded == "" {
			continue
		}
		token, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		roles[role] = string(token)
	}
	return roles
}

// createPodLocked makes the pod the Sandbox's podTemplate describes, Ready: the controller's own
// pod, owned by the Sandbox, carrying its labels, with every init container completed and every
// container running, as sandbox/rig_test.go's createPod and runningStatusOf build it. Its
// creationTimestamp is the one an API server stamps and serves, to the second: the daemon reads
// it to decide whether a release's saved work is this pod's to restore, and the pod a wake makes
// after the save is the one that may take it (store.TakeWorkspaceBundleForRestore).
func (a *issueSandboxAPI) createPodLocked(name string, sandbox map[string]any) string {
	spec, _ := sandbox["spec"].(map[string]any)
	template, _ := kubeClone(spec)["podTemplate"].(map[string]any)
	if template == nil {
		return ""
	}
	podSpec, _ := template["spec"].(map[string]any)
	if podSpec == nil {
		return ""
	}
	meta, _ := sandbox["metadata"].(map[string]any)
	uid, _ := meta["uid"].(string)
	labels := map[string]any{}
	if templateMeta, _ := template["metadata"].(map[string]any); templateMeta != nil {
		if written, _ := templateMeta["labels"].(map[string]any); written != nil {
			labels = written
		}
	}
	a.uids++
	podUID := fmt.Sprintf("uid-pod-%d", a.uids)
	podSpec["nodeName"] = "ip-192-0-2-7"
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": sandboxNamespace, "labels": labels,
			"uid": podUID, "creationTimestamp": time.Now().UTC().Format(time.RFC3339),
			"ownerReferences": []any{map[string]any{
				"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox", "name": name, "uid": uid, "controller": true,
			}},
		},
		"spec":   podSpec,
		"status": sandboxPodStatus(podSpec),
	}
	a.putLocked("pods", name, pod)
	return podUID
}

// sandboxPodStatus is a Running pod's status for spec: every init container completed and every
// container running (sandbox/rig_test.go's runningStatusOf).
func sandboxPodStatus(podSpec map[string]any) map[string]any {
	status := map[string]any{"phase": "Running"}
	var inits []any
	if containers, _ := podSpec["initContainers"].([]any); containers != nil {
		for _, c := range containers {
			container, _ := c.(map[string]any)
			name, _ := container["name"].(string)
			inits = append(inits, map[string]any{
				"name": name, "ready": false, "restartCount": float64(0), "image": container["image"], "imageID": "",
				"state": map[string]any{"terminated": map[string]any{"exitCode": float64(0), "reason": "Completed"}},
			})
		}
	}
	if inits != nil {
		status["initContainerStatuses"] = inits
	}
	var running []any
	if containers, _ := podSpec["containers"].([]any); containers != nil {
		for _, c := range containers {
			container, _ := c.(map[string]any)
			name, _ := container["name"].(string)
			running = append(running, map[string]any{
				"name": name, "ready": true, "restartCount": float64(0), "image": container["image"], "imageID": "",
				"state": map[string]any{"running": map[string]any{"startedAt": time.Now().UTC().Format(time.RFC3339)}},
			})
		}
	}
	if running != nil {
		status["containerStatuses"] = running
	}
	return status
}

// controllerService is the headless Service the Agent Sandbox controller makes for a Sandbox that
// asks for one, as outbox_issue_suspend_test.go's controllerService builds it.
func controllerService(name, uid string) map[string]any {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(name))
	label := map[string]any{"agents.x-k8s.io/sandbox-name-hash": fmt.Sprintf("%08x", hash.Sum32())}
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": name, "namespace": sandboxNamespace, "labels": label,
			"ownerReferences": []any{map[string]any{"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox", "name": name, "uid": uid, "controller": true}}},
		"spec": map[string]any{"clusterIP": "None", "selector": label},
	}
}

// putLocked writes a copy of object as resource's name at a fresh resourceVersion and tells every
// watcher. The copy is what is stored and what the watchers carry, and nothing writes it again, so
// a watcher encoding an event never reads a map another request is editing.
func (a *issueSandboxAPI) putLocked(resource, name string, object map[string]any) map[string]any {
	a.rv++
	stored := kubeClone(object)
	meta, _ := stored["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		stored["metadata"] = meta
	}
	_, existed := a.objects[resource][name]
	meta["resourceVersion"] = fmt.Sprint(a.rv)
	a.objects[resource][name] = stored
	kind := "ADDED"
	if existed {
		kind = "MODIFIED"
	}
	a.notifyLocked(resource, kubeEvent{Type: kind, Object: stored})
	return stored
}

// kubeClone is a deep copy of a decoded JSON object.
func kubeClone(object map[string]any) map[string]any {
	if object == nil {
		return nil
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return map[string]any{}
	}
	var copied map[string]any
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return map[string]any{}
	}
	return copied
}

// deleteLocked removes resource's name and tells every watcher.
func (a *issueSandboxAPI) deleteLocked(resource, name string) {
	object, ok := a.objects[resource][name]
	if !ok {
		return
	}
	a.rv++
	delete(a.objects[resource], name)
	a.notifyLocked(resource, kubeEvent{Type: "DELETED", Object: object})
}

func (a *issueSandboxAPI) notifyLocked(resource string, event kubeEvent) {
	for _, watch := range a.watches[resource] {
		select {
		case watch <- event:
		default:
		}
	}
}

// sandbox is the Sandbox named name as the API holds it, or nil.
func (a *issueSandboxAPI) sandbox(name string) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.objects["sandboxes"][name]
}

// pod is whether a pod of name exists.
func (a *issueSandboxAPI) pod(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.objects["pods"][name]
	return ok
}

// mode is the Sandbox's operatingMode, "deleted" once the API no longer holds it.
func (a *issueSandboxAPI) mode(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	object, ok := a.objects["sandboxes"][name]
	if !ok {
		return "deleted"
	}
	spec, _ := object["spec"].(map[string]any)
	mode, _ := spec["operatingMode"].(string)
	if mode == "" {
		return "Running"
	}
	return mode
}

// label is the Sandbox's label key, "" once the API no longer has it.
func (a *issueSandboxAPI) label(name, key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	object, ok := a.objects["sandboxes"][name]
	if !ok {
		return ""
	}
	labels, _ := object["metadata"].(map[string]any)["labels"].(map[string]any)
	value, _ := labels[key].(string)
	return value
}

// uid is the Sandbox's uid, "" once the API no longer has it.
func (a *issueSandboxAPI) uid(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	object, ok := a.objects["sandboxes"][name]
	if !ok {
		return ""
	}
	uid, _ := object["metadata"].(map[string]any)["uid"].(string)
	return uid
}

// disk is whether the disk of the Sandbox's pod, the claim that pod owns, still exists.
func (a *issueSandboxAPI) disk(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.disks[name]
}

// deleted is the options of the Sandbox's delete, and whether one was requested.
func (a *issueSandboxAPI) deleted(name string) (metav1.DeleteOptions, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	options, ok := a.deletes[name]
	return options, ok
}

// initEnv is the environment variable names of the Sandbox's workspace-init container, from the
// pod template the daemon's Running patch wrote; nil when the Sandbox has no template.
func (a *issueSandboxAPI) initEnv(name string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	object, ok := a.objects["sandboxes"][name]
	if !ok {
		return nil
	}
	template, _ := object["spec"].(map[string]any)["podTemplate"].(map[string]any)
	if template == nil {
		return nil
	}
	containers, _ := template["spec"].(map[string]any)["initContainers"].([]any)
	var names []string
	for _, c := range containers {
		container := c.(map[string]any)
		if container["name"] != "workspace-init" {
			continue
		}
		env, _ := container["env"].([]any)
		for _, e := range env {
			variable, _ := e.(map[string]any)
			name, _ := variable["name"].(string)
			names = append(names, name)
		}
	}
	return names
}

// requireServed fails the test naming every request the API did not know.
func (a *issueSandboxAPI) requireServed(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.unexpected) > 0 {
		t.Errorf("the rig's Kubernetes API did not serve %v", a.unexpected)
	}
}

func (a *issueSandboxAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	resource, name, ok := kubePath(r.URL.Path)
	if !ok {
		a.unserved(w, r)
		return
	}
	if resource == "persistentvolumeclaims" {
		a.mu.Lock()
		a.pvcRequests++
		a.mu.Unlock()
		kubeStatus(w, http.StatusForbidden, metav1.StatusReasonForbidden)
		return
	}
	kind, known := kubeResourceOf(resource)
	if !known {
		a.unserved(w, r)
		return
	}
	if r.URL.Query().Get("watch") == "true" {
		a.watch(w, r, kind)
		return
	}
	switch {
	case r.Method == http.MethodGet && name == "":
		a.list(w, r, kind)
	case r.Method == http.MethodGet:
		a.get(w, kind, name)
	case r.Method == http.MethodPost && name == "":
		a.create(w, r, kind)
	case r.Method == http.MethodPut:
		a.replace(w, r, kind, name)
	case r.Method == http.MethodPatch:
		a.patch(w, r, kind, name)
	case r.Method == http.MethodDelete:
		a.remove(w, r, kind, name)
	default:
		a.unserved(w, r)
	}
}

func (a *issueSandboxAPI) unserved(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.unexpected = append(a.unexpected, r.Method+" "+r.URL.Path)
	a.mu.Unlock()
	kubeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound)
}

// kubePath is the resource and object name of a namespaced Kubernetes path, for the two API roots
// the runtime uses: /api/v1/namespaces/<ns>/<resource>[/<name>] and
// /apis/<group>/<version>/namespaces/<ns>/<resource>[/<name>].
func kubePath(path string) (resource, name string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, part := range parts {
		if part != "namespaces" || i+2 >= len(parts) {
			continue
		}
		rest := parts[i+2:]
		switch len(rest) {
		case 1:
			return rest[0], "", true
		case 2:
			return rest[0], rest[1], true
		}
		return "", "", false
	}
	return "", "", false
}

func kubeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status: "Failure", Reason: reason, Code: int32(code)})
}

func (a *issueSandboxAPI) list(w http.ResponseWriter, r *http.Request, kind kubeResource) {
	a.mu.Lock()
	items := []any{}
	for _, object := range a.objects[kind.resource] {
		if kubeSelected(object, r.URL.Query().Get("labelSelector")) {
			items = append(items, object)
		}
	}
	rv := fmt.Sprint(a.rv)
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"apiVersion": kind.apiVersion, "kind": kind.kind + "List",
		"metadata": map[string]any{"resourceVersion": rv}, "items": items,
	})
}

func (a *issueSandboxAPI) get(w http.ResponseWriter, kind kubeResource, name string) {
	a.mu.Lock()
	object, ok := a.objects[kind.resource][name]
	a.mu.Unlock()
	if !ok {
		kubeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(object)
}

func (a *issueSandboxAPI) create(w http.ResponseWriter, r *http.Request, kind kubeResource) {
	var object map[string]any
	if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
		a.mu.Lock()
		a.unexpected = append(a.unexpected, fmt.Sprintf("POST %s (%s): %v", r.URL.Path, r.Header.Get("Content-Type"), err))
		a.mu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	meta, _ := object["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	a.mu.Lock()
	if _, taken := a.objects[kind.resource][name]; taken {
		a.mu.Unlock()
		kubeStatus(w, http.StatusConflict, metav1.StatusReasonAlreadyExists)
		return
	}
	a.uids++
	meta["uid"], meta["generation"] = fmt.Sprintf("uid-%s-%d", kind.resource, a.uids), float64(1)
	meta["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
	object["apiVersion"], object["kind"] = kind.apiVersion, kind.kind
	stored := a.putLocked(kind.resource, name, object)
	if kind.resource == "sandboxes" {
		a.disks[name], a.created = true, append(a.created, name)
		// The controller makes the headless Service of a Sandbox that asks for one as soon as the
		// Sandbox arrives, so a test that runs no controller loop still sees it.
		if spec, _ := stored["spec"].(map[string]any); spec["service"] == true {
			if _, made := a.objects["services"][name]; !made {
				uid, _ := stored["metadata"].(map[string]any)["uid"].(string)
				a.putLocked("services", name, controllerService(name, uid))
			}
		}
	}
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(stored)
}

func (a *issueSandboxAPI) replace(w http.ResponseWriter, r *http.Request, kind kubeResource, name string) {
	var object map[string]any
	if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	if _, ok := a.objects[kind.resource][name]; !ok {
		a.mu.Unlock()
		kubeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound)
		return
	}
	meta, _ := object["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{"name": name}
		object["metadata"] = meta
	}
	object["apiVersion"], object["kind"] = kind.apiVersion, kind.kind
	stored := a.putLocked(kind.resource, name, object)
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stored)
}

// patch applies the runtime's JSON patch (a Sandbox's operatingMode and labels, each with its
// `test` preconditions) or a JSON merge patch (a role's address record, a suspension naming a
// displaced pod), as outbox_issue_suspend_test.go's stand-in does.
func (a *issueSandboxAPI) patch(w http.ResponseWriter, r *http.Request, kind kubeResource, name string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	held, ok := a.objects[kind.resource][name]
	if !ok {
		kubeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound)
		return
	}
	object := kubeClone(held)
	meta, _ := object["metadata"].(map[string]any)
	if strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		var merge map[string]any
		if err := json.Unmarshal(body, &merge); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		kubeMerge(object, merge)
	} else {
		var ops []struct {
			Op    string `json:"op"`
			Path  string `json:"path"`
			Value any    `json:"value"`
		}
		if err := json.Unmarshal(body, &ops); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		spec, _ := object["spec"].(map[string]any)
		for _, op := range ops {
			switch {
			case op.Op == "test" && op.Path == "/metadata/uid" && op.Value == meta["uid"]:
			case op.Op == "test" && op.Path == "/metadata/resourceVersion" && op.Value == meta["resourceVersion"]:
			case op.Op == "test":
				http.Error(w, "stale patch: "+op.Path, http.StatusConflict)
				return
			case op.Op == "add" && strings.HasPrefix(op.Path, "/spec/"):
				spec[kubePointer(strings.TrimPrefix(op.Path, "/spec/"))] = op.Value
				meta["generation"] = kubeGeneration(meta) + 1
			case op.Op == "add" && strings.HasPrefix(op.Path, "/metadata/labels/"):
				labels, _ := meta["labels"].(map[string]any)
				if labels == nil {
					labels = map[string]any{}
					meta["labels"] = labels
				}
				labels[kubePointer(strings.TrimPrefix(op.Path, "/metadata/labels/"))] = op.Value
			case op.Op == "add" && strings.HasPrefix(op.Path, "/metadata/annotations/"):
				annotations, _ := meta["annotations"].(map[string]any)
				if annotations == nil {
					annotations = map[string]any{}
					meta["annotations"] = annotations
				}
				annotations[kubePointer(strings.TrimPrefix(op.Path, "/metadata/annotations/"))] = op.Value
			default:
				http.Error(w, "unexpected patch "+op.Op+" "+op.Path, http.StatusConflict)
				return
			}
		}
	}
	stored := a.putLocked(kind.resource, name, object)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stored)
}

// kubePointer is one JSON Pointer token with its escapes undone.
func kubePointer(token string) string {
	return strings.NewReplacer("~1", "/", "~0", "~").Replace(token)
}

func kubeGeneration(meta map[string]any) float64 {
	generation, _ := meta["generation"].(float64)
	return generation
}

// kubeMerge applies a JSON merge patch to object.
func kubeMerge(object, patch map[string]any) {
	for key, value := range patch {
		nested, isObject := value.(map[string]any)
		if target, ok := object[key].(map[string]any); ok && isObject {
			kubeMerge(target, nested)
			continue
		}
		object[key] = value
	}
}

func (a *issueSandboxAPI) remove(w http.ResponseWriter, r *http.Request, kind kubeResource, name string) {
	var options metav1.DeleteOptions
	if err := json.NewDecoder(r.Body).Decode(&options); err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	object, ok := a.objects[kind.resource][name]
	if !ok {
		a.mu.Unlock()
		kubeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound)
		return
	}
	meta, _ := object["metadata"].(map[string]any)
	if p := options.Preconditions; p != nil &&
		(p.UID != nil && string(*p.UID) != meta["uid"] || p.ResourceVersion != nil && *p.ResourceVersion != meta["resourceVersion"]) {
		a.mu.Unlock()
		kubeStatus(w, http.StatusConflict, metav1.StatusReasonConflict)
		return
	}
	a.deleteLocked(kind.resource, name)
	if kind.resource == "sandboxes" {
		a.deletes[name] = options
		if options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground {
			delete(a.disks, name)
		}
	}
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Success"})
}

// watch streams resource's events until the request's context ends. A watch that asks for the
// initial events (client-go's watch-list) gets an ADDED for every object it selects and then the
// `k8s.io/initial-events-end` bookmark, which is what tells the informer its store is complete.
func (a *issueSandboxAPI) watch(w http.ResponseWriter, r *http.Request, kind kubeResource) {
	selector := r.URL.Query().Get("labelSelector")
	events := make(chan kubeEvent, 256)
	a.mu.Lock()
	var initial []map[string]any
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		for _, object := range a.objects[kind.resource] {
			if kubeSelected(object, selector) {
				initial = append(initial, object)
			}
		}
	}
	rv := fmt.Sprint(a.rv)
	a.watches[kind.resource] = append(a.watches[kind.resource], events)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		kept := a.watches[kind.resource][:0]
		for _, watch := range a.watches[kind.resource] {
			if watch != events {
				kept = append(kept, watch)
			}
		}
		a.watches[kind.resource] = kept
		a.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	write := func(event kubeEvent) bool {
		if err := encoder.Encode(event); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		for _, object := range initial {
			if !write(kubeEvent{Type: "ADDED", Object: object}) {
				return
			}
		}
		if !write(kubeEvent{Type: "BOOKMARK", Object: map[string]any{
			"apiVersion": kind.apiVersion, "kind": kind.kind,
			"metadata": map[string]any{"resourceVersion": rv, "annotations": map[string]any{"k8s.io/initial-events-end": "true"}},
		}}) {
			return
		}
	} else if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-events:
			if !kubeSelected(event.Object, selector) {
				continue
			}
			if !write(event) {
				return
			}
		}
	}
}

// kubeSelected is whether object's labels hold every `key=value` term of selector, an empty
// selector selecting every object (outbox_issue_suspend_test.go's selected).
func kubeSelected(object map[string]any, selector string) bool {
	meta, _ := object["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	for _, term := range strings.Split(selector, ",") {
		if term == "" {
			continue
		}
		key, value, _ := strings.Cut(term, "=")
		if labels[key] != value {
			return false
		}
	}
	return true
}
