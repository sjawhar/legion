package sandbox

import (
	"context"
	"errors"
	"fmt"

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
// the Deployment has no available replica.
func CheckInstalled(ctx context.Context, rc *rest.Config, ref InstallRef) error {
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("agent sandbox check: dynamic client: %w", err)
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("agent sandbox check: kubernetes client: %w", err)
	}
	return checkInstalled(ctx, dyn, kube, ref)
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
