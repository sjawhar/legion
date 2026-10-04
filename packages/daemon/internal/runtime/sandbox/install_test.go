package sandbox

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var productionInstall = InstallRef{CRD: "sandboxes.agents.x-k8s.io", ControllerNamespace: "agent-sandbox-system", ControllerName: "agent-sandbox-controller"}

func crdObject(served bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": productionInstall.CRD},
		"spec":     map[string]any{"versions": []any{map[string]any{"name": "v1beta1", "served": served, "storage": true}}},
	}}
}

func controllerDeployment(available int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: productionInstall.ControllerName, Namespace: productionInstall.ControllerNamespace},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: available},
	}
}

func forbid(resource string) k8stesting.ReactionFunc {
	return func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		get := a.(k8stesting.GetAction)
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: a.GetResource().Group, Resource: resource}, get.GetName(),
			nil)
	}
}

// The boot refusal names every missing piece, whether the API says it is absent, forbids reading
// it — under a resourceNames grant a Deployment that does not exist is forbidden, not absent — or
// the controller has no available replica (N8); an installed Agent Sandbox passes.
func TestCheckInstalledNamesEveryMissingPiece(t *testing.T) {
	crdPiece := "the Sandbox CRD sandboxes.agents.x-k8s.io"
	controllerPiece := "the Agent Sandbox controller Deployment agent-sandbox-system/agent-sandbox-controller"
	for name, tc := range map[string]struct {
		crd        *unstructured.Unstructured
		deployment *appsv1.Deployment
		forbidCRD  bool
		forbidDep  bool
		want       []string
	}{
		"installed":          {crd: crdObject(true), deployment: controllerDeployment(1)},
		"no CRD":             {deployment: controllerDeployment(1), want: []string{crdPiece + " is not installed (404)"}},
		"CRD forbidden":      {crd: crdObject(true), deployment: controllerDeployment(1), forbidCRD: true, want: []string{crdPiece + " cannot be read (403)"}},
		"v1beta1 not served": {crd: crdObject(false), deployment: controllerDeployment(1), want: []string{crdPiece + " does not serve v1beta1"}},
		"no controller":      {crd: crdObject(true), want: []string{controllerPiece + " is not installed (404)"}},
		"controller forbidden": {
			crd: crdObject(true), deployment: controllerDeployment(1), forbidDep: true, want: []string{controllerPiece + " cannot be read (403)"},
		},
		"no available replica": {crd: crdObject(true), deployment: controllerDeployment(0), want: []string{controllerPiece + " has 0 available replicas"}},
		"nothing":              {want: []string{crdPiece + " is not installed", controllerPiece + " is not installed"}},
	} {
		t.Run(name, func(t *testing.T) {
			var crds []*unstructured.Unstructured
			if tc.crd != nil {
				crds = append(crds, tc.crd)
			}
			dyn := newDynamic(t, crds...)
			kube := kubefake.NewClientset()
			if tc.deployment != nil {
				kube = kubefake.NewClientset(tc.deployment)
			}
			if tc.forbidCRD {
				dyn.PrependReactor("get", "customresourcedefinitions", forbid("customresourcedefinitions"))
			}
			if tc.forbidDep {
				kube.PrependReactor("get", "deployments", forbid("deployments"))
			}
			err := checkInstalled(context.Background(), dyn, kube, productionInstall)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("an installed Agent Sandbox refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("passed; want a refusal naming %v", tc.want)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not say %q", err, want)
				}
			}
		})
	}
}
