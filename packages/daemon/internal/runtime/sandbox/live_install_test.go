//go:build e2e

// The Stage 4a harness's identity and install checks: who the runtime is, what it may do, and
// that the cluster has Agent Sandbox and the worker image passes its probe. The rig is
// live_test.go.

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/prompts"
)

// identity: the runtime's client is the restricted Legion daemon identity, and nothing more.
func (r *liveRig) checkIdentity() error {
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Minute)
	defer cancel()
	review, err := r.kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("SelfSubjectReview through the runtime context: %w", err)
	}
	user := review.Status.UserInfo
	out, err := r.kubectl("auth", "whoami", "-o", "json")
	if err != nil {
		return err
	}
	var operator authnv1.SelfSubjectReview
	if err := json.Unmarshal([]byte(out), &operator); err != nil {
		return fmt.Errorf("the operator's whoami: %w", err)
	}
	note("runtime", "SelfSubjectReview: user %s, groups %v", user.Username, user.Groups)
	note("operator", "whoami: user %s", operator.Status.UserInfo.Username)
	if user.Username == operator.Status.UserInfo.Username {
		return fmt.Errorf("the runtime context authenticates as the operator (%s); refusing to start", user.Username)
	}
	if !regexp.MustCompile(`:assumed-role/[A-Za-z0-9+=,.@_-]*legion-daemon/`).MatchString(user.Username) {
		return fmt.Errorf("the runtime identity %s is not the assumed Legion daemon role", user.Username)
	}
	if !slices.Contains(user.Groups, "legion-daemon") {
		return fmt.Errorf("the runtime identity is not in group legion-daemon: %v", user.Groups)
	}

	_, err = r.kube.CoreV1().Secrets(r.env.namespace).List(ctx, metav1.ListOptions{Limit: 1})
	if !apierrors.IsForbidden(err) {
		return fmt.Errorf("list secrets in %s through the runtime identity answered %v, want 403", r.env.namespace, err)
	}
	note("runtime", "list secrets -n %s: 403 Forbidden", r.env.namespace)

	// can-i --list, which is a SelfSubjectRulesReview, in every namespace: an RBAC grant anywhere
	// that the plan does not make fails the check, a stray RoleBinding in another namespace
	// included.
	out, err = r.kubectl("get", "namespaces", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return err
	}
	namespaces := strings.Fields(out)
	var extra []string
	incomplete := ""
	for _, ns := range namespaces {
		rules, err := r.kube.AuthorizationV1().SelfSubjectRulesReviews().Create(ctx, &authzv1.SelfSubjectRulesReview{
			Spec: authzv1.SelfSubjectRulesReviewSpec{Namespace: ns},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("rules review in %s: %w", ns, err)
		}
		if rules.Status.Incomplete {
			incomplete = rules.Status.EvaluationError
		}
		for _, rule := range rules.Status.ResourceRules {
			for _, g := range expand(rule) {
				if !planGrants(ns, g) {
					extra = append(extra, ns+": "+g)
				}
			}
		}
		for _, rule := range rules.Status.NonResourceRules {
			if len(rule.Verbs) != 1 || rule.Verbs[0] != "get" {
				extra = append(extra, fmt.Sprintf("%s: %v on %v", ns, rule.Verbs, rule.NonResourceURLs))
			}
		}
	}
	note("operator", "%d namespaces listed", len(namespaces))
	if len(extra) > 0 {
		return fmt.Errorf("the runtime identity holds %d grants the plan does not make: %s", len(extra), strings.Join(extra[:min(len(extra), 20)], "; "))
	}
	note("runtime", "rules review (can-i --list) in all %d namespaces: every resource grant is the plan's, every non-resource grant is get", len(namespaces))
	if incomplete != "" {
		note("runtime", "the API server marks the rules incomplete (%s); the access reviews below ask the webhook too", incomplete)
	}

	// Access reviews go through every authorizer, EKS's webhook included, which a rules review
	// cannot enumerate. Cluster-wide, and a positive control each for the two grants that must hold.
	type question struct {
		allow bool
		attrs authzv1.ResourceAttributes
	}
	questions := []question{
		{true, authzv1.ResourceAttributes{Namespace: r.env.namespace, Verb: "create", Group: "agents.x-k8s.io", Resource: "sandboxes"}},
		{true, authzv1.ResourceAttributes{Namespace: r.env.namespace, Verb: "watch", Resource: "services"}},
		{true, authzv1.ResourceAttributes{Namespace: "agent-sandbox-system", Verb: "get", Group: "apps", Resource: "deployments", Name: "agent-sandbox-controller"}},
	}
	for _, resource := range []string{"users", "groups", "serviceaccounts", "uids"} {
		questions = append(questions, question{false, authzv1.ResourceAttributes{Verb: "impersonate", Resource: resource}})
	}
	questions = append(questions,
		question{false, authzv1.ResourceAttributes{Verb: "impersonate", Group: "authentication.k8s.io", Resource: "userextras"}},
		question{false, authzv1.ResourceAttributes{Verb: "create", Resource: "serviceaccounts", Subresource: "token"}},
		question{false, authzv1.ResourceAttributes{Verb: "create", Resource: "pods"}},
		question{false, authzv1.ResourceAttributes{Verb: "create", Resource: "pods", Subresource: "exec"}},
		question{false, authzv1.ResourceAttributes{Verb: "list", Resource: "secrets"}},
		question{false, authzv1.ResourceAttributes{Verb: "create", Resource: "secrets"}},
		question{false, authzv1.ResourceAttributes{Verb: "get", Resource: "persistentvolumeclaims"}},
		question{false, authzv1.ResourceAttributes{Verb: "list", Resource: "nodes"}},
		question{false, authzv1.ResourceAttributes{Verb: "create", Group: "agents.x-k8s.io", Resource: "sandboxes"}},
	)
	for _, resource := range []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"} {
		for _, verb := range []string{"create", "update", "patch", "escalate", "bind"} {
			questions = append(questions, question{false, authzv1.ResourceAttributes{Verb: verb, Group: "rbac.authorization.k8s.io", Resource: resource}})
		}
	}
	allowed, denied := 0, 0
	for _, q := range questions {
		review, err := r.kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authzv1.SelfSubjectAccessReview{
			Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &q.attrs},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("access review %+v: %w", q.attrs, err)
		}
		what := describeAttrs(q.attrs)
		if review.Status.Allowed != q.allow {
			return fmt.Errorf("access review %s answered allowed=%t, want %t (%s)", what, review.Status.Allowed, q.allow, review.Status.Reason)
		}
		if q.allow {
			allowed++
		} else {
			denied++
		}
	}
	note("runtime", "access reviews: %d positive controls allowed; %d denied cluster-wide (impersonation of every kind, serviceaccounts/token, pods and pods/exec, secrets list and create, PVC get, nodes, RBAC create/update/patch/escalate/bind, sandboxes outside %s)", allowed, denied, r.env.namespace)
	return nil
}

// expand is a resource rule as one "verb group/resource[ name]" per combination.
func expand(rule authzv1.ResourceRule) []string {
	groups, names := rule.APIGroups, rule.ResourceNames
	if len(groups) == 0 {
		groups = []string{""}
	}
	if len(names) == 0 {
		names = []string{""}
	}
	var out []string
	for _, verb := range rule.Verbs {
		for _, group := range groups {
			for _, resource := range rule.Resources {
				for _, name := range names {
					out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s/%s %s", verb, group, resource, name)))
				}
			}
		}
	}
	return out
}

// planGrants is the Stage 4 plan's Task 4.4 grant set, with the Service reads the repository
// cache's check needs (docs/kubernetes.md "RBAC the Go daemon needs") and the self-review endpoints
// every authenticated user has.
func planGrants(ns, grant string) bool {
	everywhere := []string{
		"create authentication.k8s.io/selfsubjectreviews",
		"create authorization.k8s.io/selfsubjectaccessreviews",
		"create authorization.k8s.io/selfsubjectrulesreviews",
		"get apiextensions.k8s.io/customresourcedefinitions sandboxes.agents.x-k8s.io",
	}
	if slices.Contains(everywhere, grant) {
		return true
	}
	switch ns {
	case "legion":
		var legion []string
		for _, verb := range []string{"create", "get", "list", "watch", "patch", "delete"} {
			legion = append(legion, verb+" agents.x-k8s.io/sandboxes")
		}
		for _, verb := range []string{"get", "list", "watch"} {
			legion = append(legion, verb+" /pods", verb+" /services")
		}
		for _, verb := range []string{"create", "get", "update", "delete"} {
			legion = append(legion, verb+" /secrets")
		}
		legion = append(legion, "get agents.x-k8s.io/sandboxes/status", "get /pods/log", "list /events")
		return slices.Contains(legion, grant)
	case "agent-sandbox-system":
		return grant == "get apps/deployments agent-sandbox-controller"
	}
	return false
}

func describeAttrs(a authzv1.ResourceAttributes) string {
	resource := a.Resource
	if a.Group != "" {
		resource += "." + a.Group
	}
	if a.Subresource != "" {
		resource += "/" + a.Subresource
	}
	if a.Name != "" {
		resource += "/" + a.Name
	}
	scope := "cluster-wide"
	if a.Namespace != "" {
		scope = "-n " + a.Namespace
	}
	return fmt.Sprintf("%s %s %s", a.Verb, resource, scope)
}

// installed: Requirement 10's accepting path, under the shipped resourceNames grants.
func (r *liveRig) checkInstalled() error {
	if err := CheckInstalled(r.ctx, r.rc, productionInstall); err != nil {
		return err
	}
	note("runtime", "CheckInstalled(%s, %s/%s): nil — the CRD serves %s and the controller has an available replica",
		productionInstall.CRD, productionInstall.ControllerNamespace, productionInstall.ControllerName, sandboxGVR.Version)
	return nil
}

// boot-refusal-negative: a controller that is not there is refused by name, although the
// resourceNames grant makes the API answer 403 rather than 404.
func (r *liveRig) checkBootRefusal() error {
	ref := productionInstall
	ref.ControllerName = "legion-no-such-controller"
	err := CheckInstalled(r.ctx, r.rc, ref)
	if err == nil {
		return errors.New("CheckInstalled accepted a controller Deployment that does not exist")
	}
	message := err.Error()
	note("runtime", "CheckInstalled refused: %s", message)
	switch {
	case !strings.Contains(message, "agent-sandbox-system/legion-no-such-controller"):
		return errors.New("the refusal does not name the Deployment agent-sandbox-system/legion-no-such-controller")
	case !strings.Contains(message, "(403)"):
		return errors.New("the refusal is not the resourceNames grant's 403")
	case strings.Contains(message, "Sandbox CRD"):
		return errors.New("the refusal also blames the CRD, which is installed")
	}
	return nil
}

// image-probe: the probe Sandbox, carrying the operator's pod, passes on the stage image, confirms
// the daemon API contract, resolved the model of every task agent the prompts dispatch, and checked
// the capability list, every image-site capability present; the report carries the model-fallback
// mark the pod read. Its pod is read while the probe runs: the one container carries the probe's
// own reservation as both request and limit (imageProbe), so the probe pod is Guaranteed whatever
// the roles request, and it asks nothing of its placement. The negative control is the same pod
// with its container's cpu limit raised past its request, which the reservation rule refuses
// naming the container.
func (r *liveRig) checkImageProbe() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	_, digest, _ := strings.Cut(r.env.image, "@sha256:")
	name := probeName(r.env.project, digest, "")
	if err := r.recordSandbox(name); err != nil {
		return err
	}
	p := r.imageProbe()
	// The probe pod lives from the controller's create until ProbeImage deletes the Sandbox, so a
	// poll started before the probe sees it in whatever phase it is in; its spec and qosClass are
	// set at creation.
	watching, stopWatching := context.WithCancel(r.ctx)
	defer stopWatching()
	probePod := make(chan *corev1.Pod, 1)
	go func() {
		defer close(probePod)
		for {
			if pod, err := r.getPod(name); err == nil {
				probePod <- pod
				return
			}
			select {
			case <-watching.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	report, err := r.rt.ProbeImage(r.ctx, p)
	if err != nil {
		return err
	}
	stopWatching()
	passed, ok := r.logs.find("sandbox runtime: the worker image passed its probe")
	if !ok {
		return errors.New("ProbeImage returned nil without logging a pass")
	}
	contract, ok := bootprobe.ConfirmedContract(passed["log"])
	if !ok || contract != api.DaemonAPIVersion {
		return fmt.Errorf("the probe log confirms contract %d (found %t), want %d: %s", contract, ok, api.DaemonAPIVersion, passed["log"])
	}
	note("runtime", "probe Sandbox %s passed: %s", passed["sandbox"], lastLine(passed["log"], bootprobe.OKPrefix))
	note("runtime", "daemon-api-version=%d parsed, the daemon's contract", contract)
	if models := bootprobe.AgentModels(passed["log"]); models != bootprobe.AgentModelsResolved {
		return fmt.Errorf("the probe log says agent-models=%q, want %q: %s", models, bootprobe.AgentModelsResolved, passed["log"])
	}
	note("runtime", "agent-models=%s parsed: every task agent the prompts dispatch resolved its model under the operator's pod", bootprobe.AgentModelsResolved)
	if !bootprobe.CapabilitiesChecked(passed["log"]) {
		return fmt.Errorf("the probe log carries no capabilities=checked: %s", passed["log"])
	}
	note("runtime", "capabilities=checked parsed: every image-site capability present under the operator's pod")
	if report.ModelFallback != "on" && report.ModelFallback != "off" {
		return fmt.Errorf("the probe reports model-fallback=%q, want on or off: %s", report.ModelFallback, passed["log"])
	}
	note("runtime", "model-fallback=%s reported: the image's retry.modelFallback under the operator's pod", report.ModelFallback)
	pod, ok := <-probePod
	if !ok || pod == nil {
		return fmt.Errorf("the probe pod %s was never read while the probe ran, so its reservation and QoS are unproven", name)
	}
	if err := guaranteedPod(pod, map[string]corev1.ResourceRequirements{probeContainer: p.Resources}, nil); err != nil {
		return fmt.Errorf("probe pod %s (phase %s): %w", name, pod.Status.Phase, err)
	}
	container := pod.Spec.Containers[0]
	note("operator", "probe pod %s read in phase %s: container %s cpu %s, memory %s, request = limit, the probe's own reservation, ephemeral-storage %s under a limit of %s; qosClass %s; no affinity",
		name, pod.Status.Phase, container.Name, container.Resources.Limits.Cpu().String(), container.Resources.Limits.Memory().String(), container.Resources.Requests.StorageEphemeral().String(), container.Resources.Limits.StorageEphemeral().String(), pod.Status.QOSClass)
	bursting := pod.DeepCopy()
	doubled := bursting.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	doubled.Add(doubled)
	bursting.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = doubled
	err = guaranteedPod(bursting, map[string]corev1.ResourceRequirements{probeContainer: p.Resources}, nil)
	if err == nil || !strings.Contains(err.Error(), "container "+probeContainer) {
		return fmt.Errorf("negative control: the reservation rule accepted the probe pod with its cpu limit raised to %s, or did not name the container: %v", doubled.String(), err)
	}
	note("runtime", "negative control: the same pod with its container's cpu limit raised to %s is refused: %v", doubled.String(), err)
	if err := r.poll(liveGoneLimit, "probe Sandbox "+name+" to be deleted", func() (bool, error) {
		_, err := r.getSandbox(name)
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	}); err != nil {
		return err
	}
	note("runtime", "probe Sandbox %s deleted after the attempt", name)
	return nil
}

// image-probe-negative: with modelRoles.oracle gone from the run's ConfigMap, the probe on the same
// image refuses, naming the agent and the role no one configured, since the task tool would run
// every oracle consult on the session's model; the ConfigMap is restored before the check ends,
// and every later check boots on it.
func (r *liveRig) checkImageProbeRefusal() error {
	name := r.env.operatorConfigMap
	overlay, err := r.kubectl("get", "configmap", name, "-o", `jsonpath={.data.overlay\.yml}`)
	if err != nil {
		return err
	}
	var kept []string
	for line := range strings.Lines(overlay) {
		if !strings.HasPrefix(line, "  oracle: ") {
			kept = append(kept, line)
		}
	}
	if removed := strings.Count(overlay, "\n") - strings.Count(strings.Join(kept, ""), "\n"); removed != 1 {
		return fmt.Errorf("ConfigMap %s's overlay.yml holds %d modelRoles.oracle lines, want 1", name, removed)
	}
	patch := func(text string) error {
		body, err := json.Marshal(map[string]any{"data": map[string]string{"overlay.yml": text}})
		if err != nil {
			return err
		}
		_, err = r.kubectl("patch", "configmap", name, "--type", "merge", "-p", string(body))
		return err
	}
	if err := patch(strings.Join(kept, "")); err != nil {
		return err
	}
	note("operator", "ConfigMap %s: modelRoles.oracle removed from overlay.yml", name)
	p := r.imageProbe()
	_, refusal := r.rt.ProbeImage(r.ctx, p)
	if err := patch(overlay); err != nil {
		return fmt.Errorf("restore ConfigMap %s: %w", name, err)
	}
	note("operator", "ConfigMap %s: overlay.yml restored", name)
	if refusal == nil {
		return errors.New("ProbeImage passed with modelRoles.oracle removed from the operator's overlay")
	}
	for _, want := range []string{"task agent oracle (dispatched by ", "on its model @oracle: role oracle is not configured"} {
		if !strings.Contains(refusal.Error(), want) {
			return fmt.Errorf("the probe's refusal does not say %q: %v", want, refusal)
		}
	}
	note("runtime", "refused: %s", lineContaining(refusal.Error(), "role oracle is not configured"))
	_, digest, _ := strings.Cut(r.env.image, "@sha256:")
	probe := probeName(r.env.project, digest, "")
	if err := r.poll(liveGoneLimit, "probe Sandbox "+probe+" to be deleted", func() (bool, error) {
		_, err := r.getSandbox(probe)
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	}); err != nil {
		return err
	}
	note("runtime", "probe Sandbox %s deleted after the attempt", probe)
	return nil
}

// image-probe-capability-negative: with PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium in the
// operator's pod env, the probe on the same image refuses naming the browser capability missing:
// the probe honours the variable as Oh My Pi does, so a wrong value in an operator's pod env is
// caught at boot rather than in a worker's first browser call. The probe runs on a second runtime,
// built from the run's Options with that variable added to a copy of the pod env, so the shared
// runtime's pod is never changed; it shares the run's listener, which no probe uses, and ends with
// the check.
func (r *liveRig) checkImageProbeCapabilityRefusal() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	opts := r.runtimeOptions(r.streamAddress(), r.ln)
	env := maps.Clone(r.pod.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["PUPPETEER_EXECUTABLE_PATH"] = "/nonexistent/chromium"
	opts.Pod.Env = env
	ctx, stop := context.WithCancel(r.ctx)
	defer stop()
	rt, err := New(ctx, r.rc, opts)
	if err != nil {
		return fmt.Errorf("a runtime whose pod env sets PUPPETEER_EXECUTABLE_PATH: %w", err)
	}
	note("operator", "pod env for this probe: the fixture's %d variables and PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium", len(r.pod.Env))
	p := r.imageProbe()
	_, refusal := rt.ProbeImage(ctx, p)
	if refusal == nil {
		return errors.New("ProbeImage passed with PUPPETEER_EXECUTABLE_PATH naming nothing")
	}
	if want := "capability browser is missing"; !strings.Contains(refusal.Error(), want) {
		return fmt.Errorf("the probe's refusal does not say %q: %v", want, refusal)
	}
	note("runtime", "refused: %s", lineContaining(refusal.Error(), "capability browser is missing"))
	_, digest, _ := strings.Cut(r.env.image, "@sha256:")
	probe := probeName(r.env.project, digest, "")
	if err := r.poll(liveGoneLimit, "probe Sandbox "+probe+" to be deleted", func() (bool, error) {
		_, err := r.getSandbox(probe)
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	}); err != nil {
		return err
	}
	note("runtime", "probe Sandbox %s deleted after the attempt", probe)
	return nil
}

// image-probe-missing-volume-negative: with a volume operator-missing, from a ConfigMap
// `<operator ConfigMap>-missing` that does not exist, added to the operator's pod volumes and
// mounted at /etc/legion-operator-missing (the kubelet skips a volume no container mounts), the
// probe on the same image refuses naming the volume, the ConfigMap runtime.kubernetes.pod.volumes
// gives it from, and the kubelet's own `configmap "<name>" not found`: a FailedMount for an operator
// volume whose object is missing is read on the first event read after the pod is scheduled
// (mountRecheck), never retried to the budget. The refusal is timed from the pod's PodScheduled
// transition, read from the second runtime's store while the probe runs. The probe runs on a
// second runtime, built from the run's Options with the volume and its mount added to copies of
// the pod's slices, so the shared runtime's pod is never changed; it shares the run's listener,
// which no probe uses, and ends with the check.
func (r *liveRig) checkImageProbeMissingVolumeRefusal() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	missing := r.env.operatorConfigMap + "-missing"
	if out, err := r.kubectl("get", "configmap", missing, "-o", "name"); err == nil {
		return fmt.Errorf("ConfigMap %s exists (%s): the check relies on no ConfigMap of that name", missing, strings.TrimSpace(out))
	} else if !strings.Contains(err.Error(), "(NotFound)") {
		return err
	}
	opts := r.runtimeOptions(r.streamAddress(), r.ln)
	opts.Pod.Volumes = append(slices.Clone(r.pod.Volumes), corev1.Volume{Name: "operator-missing", VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: missing}},
	}})
	opts.Pod.VolumeMounts = append(slices.Clone(r.pod.VolumeMounts), corev1.VolumeMount{Name: "operator-missing", MountPath: "/etc/legion-operator-missing", ReadOnly: true})
	ctx, stop := context.WithCancel(r.ctx)
	defer stop()
	rt, err := New(ctx, r.rc, opts)
	if err != nil {
		return fmt.Errorf("a runtime whose pod mounts ConfigMap %s: %w", missing, err)
	}
	note("operator", "pod volumes for this probe: the fixture's %d and operator-missing from ConfigMap %s, which does not exist, mounted read-only at /etc/legion-operator-missing beside the fixture's %d mounts",
		len(r.pod.Volumes), missing, len(r.pod.VolumeMounts))
	_, digest, _ := strings.Cut(r.env.image, "@sha256:")
	probe := probeName(r.env.project, digest, "")
	// The probe pod is scheduled, then read for its FailedMount, then deleted with its Sandbox,
	// all inside ProbeImage: the last pod the second runtime's store held is kept for its
	// PodScheduled transition.
	watching, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	lastSeen := make(chan *corev1.Pod, 1)
	go func() {
		var last *corev1.Pod
		defer func() { lastSeen <- last }()
		for {
			if pod := rt.storedPod(probe); pod != nil {
				last = pod
			}
			select {
			case <-watching.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
	p := r.imageProbe()
	started := time.Now()
	_, refusal := rt.ProbeImage(ctx, p)
	refusedAt := time.Now()
	stopWatching()
	pod := <-lastSeen
	if refusal == nil {
		return fmt.Errorf("ProbeImage passed with volume operator-missing naming ConfigMap %s, which does not exist", missing)
	}
	for _, want := range []string{`cannot mount volume "operator-missing"`, `ConfigMap "` + missing + `"`} {
		if !strings.Contains(refusal.Error(), want) {
			return fmt.Errorf("the probe's refusal does not say %q: %v", want, refusal)
		}
	}
	kubelet := []string{`configmap "` + missing + `" not found`, `configmaps "` + missing + `" not found`}
	if !slices.ContainsFunc(kubelet, func(form string) bool { return strings.Contains(refusal.Error(), form) }) {
		return fmt.Errorf("the probe's refusal carries neither of the kubelet's forms %q: %v", kubelet, refusal)
	}
	if strings.Contains(refusal.Error(), "never completed within its retry budget") {
		return fmt.Errorf("the probe retried the mount to its budget instead of refusing it on the first read: %v", refusal)
	}
	if pod == nil {
		return fmt.Errorf("the probe pod %s was never read while the probe ran, so the refusal's bound from its scheduling is unproven", probe)
	}
	var scheduledAt time.Time
	if scheduled := podCondition(pod, corev1.PodScheduled); scheduled != nil && scheduled.Status == corev1.ConditionTrue {
		scheduledAt = scheduled.LastTransitionTime.Time
	}
	if scheduledAt.IsZero() {
		return fmt.Errorf("the probe pod %s as last read (phase %s) carries no PodScheduled=True condition, so the refusal's bound from its scheduling is unproven", probe, pod.Status.Phase)
	}
	sinceScheduled := refusedAt.Sub(scheduledAt)
	// The reads after scheduling fall at the first poll, then every mountRecheck; a kubelet whose
	// first mount attempt lands after the first recheck is found on the second, so the bound allows
	// two rechecks, with 15 s of slack for the poll's tick, the API server's second-truncated
	// transition time and the kubelet's first volume sync. The refusal's own claim is "within one
	// read of the kubelet's first FailedMount", which the noted durations show; this bound only says
	// the attempt was not waited out.
	if limit := 2*mountRecheck + 15*time.Second; sinceScheduled > limit {
		return fmt.Errorf("the refusal came %s after the probe pod was scheduled, past the %s two event reads every %s, the poll's tick and the kubelet's first volume sync allow", sinceScheduled.Round(time.Second), limit, mountRecheck)
	}
	note("runtime", "refused %s after ProbeImage was called, %s after the pod was scheduled: %s", refusedAt.Sub(started).Round(time.Second), sinceScheduled.Round(time.Second), lineContaining(refusal.Error(), "cannot mount volume"))
	if err := r.poll(liveGoneLimit, "probe Sandbox "+probe+" to be deleted", func() (bool, error) {
		_, err := r.getSandbox(probe)
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	}); err != nil {
		return err
	}
	note("runtime", "probe Sandbox %s deleted after the attempt", probe)
	return nil
}

// imageProbe is the probe as the daemon asks for it: its contract, the references of the role
// prompts it hands every pod (prompts.RoleReferences), and the probe's own fixed reservation
// (internal/daemon/kubernetes.go imageProbe, probeReservation) — 250m and 1Gi, not a role's share,
// so a probe pod fits a pool with little room — with cpu and memory the request and the limit
// alike, so the probe pod is Guaranteed whatever the roles request. The probe command it sends, with
// --role-references and, the run having a provider key, --provider-env-dir, is noted.
func (r *liveRig) imageProbe() ImageProbe {
	p := ImageProbe{
		Contract: api.DaemonAPIVersion, Budget: 10 * time.Minute, RoleReferences: prompts.RoleReferences(),
		Retry:     bootprobe.Retry{Initial: 15 * time.Second, Max: time.Minute, Attempts: 3},
		Resources: probeReservation(),
	}
	command := r.rt.probeManifest("probe", p, time.Now()).Spec.PodTemplate.Spec.Containers[0].Command
	note("runtime", "the probe command: %s", strings.Join(command, " "))
	return p
}

func lastLine(text, prefix string) string {
	for _, line := range slices.Backward(strings.Split(text, "\n")) {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
