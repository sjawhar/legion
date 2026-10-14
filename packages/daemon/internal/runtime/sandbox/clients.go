package sandbox

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

// clientQPS and clientBurst are the rate the runtime's Kubernetes requests are paced at, sized for
// a few hundred roles: a restart has every live role's launcher say hello again, each costing a read
// of its launcher Secret (launcherCredential), beside the requests of the launches in flight.
// docs/kubernetes.md, "The daemon's Kubernetes request rate", gives the arithmetic. A watch is never
// paced (client-go's Request.Watch).
const (
	clientQPS   = 50
	clientBurst = 100
)

// kubeClients are a dynamic and a typed client of one client configuration.
type kubeClients struct {
	dyn  dynamic.Interface
	kube kubernetes.Interface
}

// runtimeClients are the runtime's two client sets: informers for its informers' lists and
// watches, and requests for every other request it makes (clientConfigs).
type runtimeClients struct {
	informers, requests kubeClients
}

// clients are the runtime's client sets of rc (clientConfigs).
func clients(rc *rest.Config) (runtimeClients, error) {
	informerConfig, requestConfig := clientConfigs(rc)
	informers, err := clientsOf(informerConfig)
	if err != nil {
		return runtimeClients{}, err
	}
	requests, err := clientsOf(requestConfig)
	if err != nil {
		return runtimeClients{}, err
	}
	return runtimeClients{informers: informers, requests: requests}, nil
}

// clientConfigs are the runtime's two configurations of rc: the requests' (requestConfig), and the
// informers', which share the requests' limiter and have no timeout, since a timeout would end a
// watch.
func clientConfigs(rc *rest.Config) (informers, requests *rest.Config) {
	requests = requestConfig(rc)
	informers = rest.CopyConfig(requests)
	informers.Timeout = 0
	return informers, requests
}

// requestConfig is rc paced by a rate limiter of its own at clientQPS and clientBurst, each request
// bounded by apiTimeout. client-go waits on a client's limiter before it starts that client's
// timeout (rest/request.go, Request.request), so a request's wait to be let go is bounded by its
// caller's context alone, a launch's own deadline, and a launch the limiter holds back waits its
// turn rather than failing; the 30 s bound is the request's own once it is sent. Each call makes a
// limiter of its own: the runtime's two client sets share one (clientConfigs), and each boot check
// paces itself at the same rate (CheckInstalled, CensusLegacyIssueSandboxes).
func requestConfig(rc *rest.Config) *rest.Config {
	requests := rest.CopyConfig(rc)
	requests.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(clientQPS, clientBurst)
	requests.Timeout = apiTimeout
	return requests
}

func clientsOf(rc *rest.Config) (kubeClients, error) {
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return kubeClients{}, fmt.Errorf("sandbox runtime: dynamic client: %w", err)
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return kubeClients{}, fmt.Errorf("sandbox runtime: kubernetes client: %w", err)
	}
	return kubeClients{dyn: dyn, kube: kube}, nil
}
