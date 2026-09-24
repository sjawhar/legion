//go:build e2e

// The Stage 4a harness's identity and install checks: who the runtime is, what it may do, and
// that the cluster has Agent Sandbox and the worker image passes its probe. The rig is live_test.go.

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
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

// planGrants is the Stage 4 plan's Task 4.4 grant set, with the self-review endpoints every
// authenticated user has.
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
			legion = append(legion, verb+" /pods")
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

// image-probe: the probe Sandbox passes on the stage image and confirms the Go daemon API contract.
func (r *liveRig) checkImageProbe() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	_, digest, _ := strings.Cut(r.env.image, "@sha256:")
	name := probeName(r.env.project, digest)
	if err := r.recordSandbox(name); err != nil {
		return err
	}
	// Per run: a pass cached by an earlier run with the same evidence directory would skip the
	// probe this check exists to run.
	stateDir := filepath.Join(r.env.work, "probe-state-"+r.env.project)
	err := r.rt.ProbeImage(r.ctx, ImageProbe{
		Contract: api.GoDaemonAPIVersion, StateDir: stateDir, Budget: 10 * time.Minute,
		Retry: bootprobe.Retry{Initial: 15 * time.Second, Max: time.Minute, Attempts: 3}, APIServer: r.rc.Host,
	})
	if err != nil {
		return err
	}
	passed, ok := r.logs.find("sandbox runtime: the worker image passed its probe")
	if !ok {
		return errors.New("ProbeImage returned nil without logging a pass")
	}
	contract, ok := bootprobe.ConfirmedContract(passed["log"])
	if !ok || contract != api.GoDaemonAPIVersion {
		return fmt.Errorf("the probe log confirms contract %d (found %t), want %d: %s", contract, ok, api.GoDaemonAPIVersion, passed["log"])
	}
	note("runtime", "probe Sandbox %s passed: %s", passed["sandbox"], lastLine(passed["log"], bootprobe.OKPrefix))
	note("runtime", "go-daemon-api-version=%d parsed, the daemon's contract", contract)
	if err := r.poll(liveGoneLimit, "probe Sandbox "+name+" to be deleted", func() (bool, error) {
		_, err := r.getSandbox(name)
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	}); err != nil {
		return err
	}
	note("runtime", "probe Sandbox %s deleted after the attempt", name)
	return nil
}

func lastLine(text, prefix string) string {
	for _, line := range slices.Backward(strings.Split(text, "\n")) {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
