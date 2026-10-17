package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// crdGVR is CustomResourceDefinitions, read through the dynamic client: the typed
// apiextensions client would pull k8s.io/apiextensions-apiserver into the module graph.
var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// CheckInstalled is the boot refusal of LEGION-206 Requirement 10: the Sandbox CRD must exist and
// serve the version this runtime speaks, and the controller's Deployment must have an available
// replica. Each piece is checked, and every one missing is named, whether the API answered 404,
// 403 — under a resourceNames grant a piece that does not exist is forbidden, not absent — or
// the Deployment has no available replica. Its clients pace themselves and bound each request as
// the runtime's requests do (requestConfig).
func CheckInstalled(ctx context.Context, rc *rest.Config, ref InstallRef) error {
	c, err := clientsOf(requestConfig(rc))
	if err != nil {
		return err
	}
	return checkInstalled(ctx, c.dyn, c.kube, ref)
}

func checkInstalled(ctx context.Context, dyn dynamic.Interface, kube kubernetes.Interface, ref InstallRef) error {
	var errs []error
	crd, err := dyn.Resource(crdGVR).Get(ctx, ref.CRD, metav1.GetOptions{})
	if err != nil {
		errs = append(errs, refusal("the Sandbox CRD "+ref.CRD, err))
	} else if !servesVersion(crd, sandboxGVR.Version) {
		errs = append(errs, fmt.Errorf("agent sandbox: the Sandbox CRD %s does not serve %s, the version this runtime speaks",
			ref.CRD, sandboxGVR.Version))
	}
	piece := "the Agent Sandbox controller Deployment " + ref.ControllerNamespace + "/" + ref.ControllerName
	deployment, err := kube.AppsV1().Deployments(ref.ControllerNamespace).Get(ctx, ref.ControllerName, metav1.GetOptions{})
	if err != nil {
		errs = append(errs, refusal(piece, err))
	} else if deployment.Status.AvailableReplicas < 1 {
		errs = append(errs, fmt.Errorf("agent sandbox: %s has %d available replicas; the controller is not running",
			piece, deployment.Status.AvailableReplicas))
	}
	return errors.Join(errs...)
}

// refusal names piece and why it could not be read.
func refusal(piece string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("agent sandbox: %s is not installed (404): %w", piece, err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("agent sandbox: %s cannot be read (403): it is absent, or this identity may not get it: %w", piece, err)
	default:
		return fmt.Errorf("agent sandbox: %s could not be read: %w", piece, err)
	}
}

// servesVersion reports whether the CRD serves version.
func servesVersion(crd *unstructured.Unstructured, version string) bool {
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, item := range versions {
		entry, _ := item.(map[string]any)
		if entry["name"] == version && entry["served"] == true {
			return true
		}
	}
	return false
}

// CensusLegacyIssueSandboxes is the Kubernetes half of the issue-pod layout fence. Daemon boot
// runs it before the store opens, so before any schema write, image probe or reconcile: a Sandbox
// this runtime did not build must never be adopted, suspended or deleted as though it were one of
// its pods, and nothing may change what an older binary needs to clean it up. Image probes are
// explicit non-issue Sandboxes and are excluded. Its client paces itself and bounds each request as
// the runtime's requests do (requestConfig).
func CensusLegacyIssueSandboxes(ctx context.Context, rc *rest.Config, namespace, project string) error {
	dyn, err := dynamic.NewForConfig(requestConfig(rc))
	if err != nil {
		return fmt.Errorf("sandbox runtime: dynamic client: %w", err)
	}
	return rejectLegacyIssueSandboxes(ctx, dyn.Resource(sandboxGVR).Namespace(namespace), project)
}

// rejectLegacyIssueSandboxes refuses the project's Sandboxes whose pod is not one this runtime
// builds: its containers must be exactly the launcher roles of the kind its labels name
// (podKindOf), every workflow role for an issue Sandbox and the controller alone for the project
// controller's, and it must own a volume of its own, the one claim template every Sandbox made now
// carries (issueVolume). So a per-claim Sandbox of the layout before issue pods (one worker
// container), the controller Sandbox a daemon before issue pods made (role=controller, one worker
// container), a pod missing a launcher, one with a container more, a Sandbox whose labels name no
// kind, and a Sandbox of the tree-volume layout before per-issue volumes (treeVolumeLayout: no
// claim template, or the tree root's `tree` template, the controller's of that layout included) are
// each refused. A repository cache's Sandbox (labelled legion.dev/cache) is judged by its own shape
// alone (cacheSandboxShape), never as a pod of a kind, and refused, as `cache Sandbox <name>`, when
// it is not one this runtime builds. The refusal names every one of them, each with its reason, and
// how many there are, so an operator removes them all before the next boot rather than one per
// refused boot.
func rejectLegacyIssueSandboxes(ctx context.Context, sandboxes dynamic.ResourceInterface, project string) error {
	list, err := sandboxes.List(ctx, metav1.ListOptions{LabelSelector: labelProject + "=" + project})
	if err != nil {
		return fmt.Errorf("sandbox runtime: census existing issue Sandboxes before layout migration: %w", err)
	}
	var refused []string
	for _, object := range list.Items {
		if object.GetLabels()[labelProbe] != "" {
			continue
		}
		if object.GetLabels()[labelCache] != "" {
			if reason := cacheSandboxShape(object); reason != "" {
				refused = append(refused, "cache Sandbox "+object.GetName()+reason)
			}
			continue
		}
		if reason := legacyIssueSandbox(object); reason != "" {
			refused = append(refused, "legacy issue Sandbox "+object.GetName()+reason)
		} else if reason := treeVolumeLayout(object); reason != "" {
			refused = append(refused, "Sandbox "+object.GetName()+reason)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("sandbox runtime: %d of project %s's Sandboxes are not pods this runtime builds; migrate or remove each before enabling issue pods: %s",
		len(refused), project, strings.Join(refused, "; "))
}

// treeVolumeLayout is why object is a Sandbox of the tree-volume layout before per-issue volumes,
// following its name in the census's refusal, or "" when it owns a volume of its own: every
// Sandbox this runtime makes carries one claim template named issueVolume, where that layout gave
// only the tree root's Sandbox a template, named `tree`, which every child of the tree mounted, and
// its controller's Sandbox a `tree` template of its own. Such a Sandbox is never adopted: a child's
// would fit no launch and be made again empty, and a root's volume holds every clone and workspace
// of its tree, which only the drained cutover docs/kubernetes.md describes may take apart.
func treeVolumeLayout(object unstructured.Unstructured) string {
	const upgrade = "; it is of the tree-volume layout before per-issue volumes, so drain and remove it as docs/kubernetes.md's \"Upgrading a deployment with running trees\" says"
	templates, found, err := unstructured.NestedSlice(object.Object, "spec", "volumeClaimTemplates")
	if err != nil || !found || len(templates) == 0 {
		return " owns no volume" + upgrade
	}
	for _, raw := range templates {
		template, ok := raw.(map[string]any)
		if !ok {
			return " has an unreadable volume claim template" + upgrade
		}
		name, _, _ := unstructured.NestedString(template, "metadata", "name")
		if name != issueVolume {
			return fmt.Sprintf(" owns a volume claim template named %q, not %q", name, issueVolume) + upgrade
		}
	}
	return ""
}

// legacyIssueSandbox is why object is not a pod this runtime builds, following its name in the
// census's refusal, or "" when it is one.
func legacyIssueSandbox(object unstructured.Unstructured) string {
	kind, err := podKindOf(object.GetLabels())
	if err != nil {
		return ": " + err.Error()
	}
	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "podTemplate", "spec", "containers")
	if err != nil || !found {
		return " has no issue-pod container shape"
	}
	var names []string
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			return " has an unreadable container shape"
		}
		name, _ := container["name"].(string)
		names = append(names, name)
	}
	roles := kind.roles()
	want := make([]string, 0, len(roles))
	for _, role := range roles {
		want = append(want, string(role))
	}
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		return fmt.Sprintf(" runs containers %v, not exactly the launchers its labels name %v", names, want)
	}
	return ""
}
