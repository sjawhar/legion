package gcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The component sizes the app pod's connections by envoy-dispatch's own count: a counted pool of
// the server's store that grows without dispatchConnectionsPerPod following it fails here.
func TestDispatchConnectionsPerPodIsTheServers(t *testing.T) {
	require.Equal(t, store.ConnectionsPerProcess, dispatchConnectionsPerPod,
		"dispatchConnectionsPerPod must be envoy-dispatch's store.ConnectionsPerProcess")
}
