package gcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The default tier's replica bound rests on how many connections one app pod opens. A pool that
// grows in the server's store without dispatchConnectionsPerPod following it leaves the bound,
// and every document that names it, overstated; this fails then. The bound itself must be the
// largest replica count whose rolling update, replicas plus one pod, stays within the connections
// the default tier leaves Dispatch: README.md and the changelog state it, so a change of it
// changes them.
func TestDispatchConnectionsPerPodIsTheServers(t *testing.T) {
	require.Equal(t, store.ConnectionsPerProcess, dispatchConnectionsPerPod,
		"dispatchConnectionsPerPod must be envoy-dispatch's store.ConnectionsPerProcess")
	usable := dispatchDefaultTierMaxConnections - postgresSuperuserReservedConnections
	require.LessOrEqual(t, (dispatchDefaultTierRollingReplicas+1)*store.ConnectionsPerProcess, usable,
		"a rolling update at the stated bound opens more connections than the default tier leaves Dispatch")
	require.Greater(t, (dispatchDefaultTierRollingReplicas+2)*store.ConnectionsPerProcess, usable,
		"the default tier holds more replicas than the stated bound")
	require.Equal(t, 4, dispatchDefaultTierRollingReplicas,
		"README.md and packages/envoy/CHANGELOG.md state 4 replicas: change them with the bound")
}
