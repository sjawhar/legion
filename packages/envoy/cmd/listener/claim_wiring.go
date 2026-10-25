package main

import "github.com/sjawhar/envoy/internal/session"

// subscribeBody is the POST /v1/interests/subscribe payload sent by each
// agent session's envoy extension, on subscribe and on every heartbeat.
type subscribeBody struct {
	SessionID    string   `json:"session_id"`
	Dir          string   `json:"dir"`
	Topics       []string `json:"topics"`
	Port         int      `json:"port"`
	Title        string   `json:"title"`
	Capabilities []string `json:"capabilities"`
	// Driving reports that this process is the one actually driving the session,
	// rather than a process that merely has it loaded from shared on-disk state.
	// Absent (older plugins) means "not driving", which is the safe default: a
	// claim without the flag cannot displace a live driving claim.
	Driving        bool `json:"driving"`
	SelfSubscribed bool `json:"self_subscribed"`
	// Host is envoy-client's machineID() (ENVOY_MACHINE_ID, falling back to the kernel
	// hostname): where the session process actually runs, as opposed to machineID below,
	// which is always the registering listener's own machine (unrelated to the caller). It is the
	// address the registering listener reaches the session's serving port at, unless that
	// listener has a host bridge (ENVOY_HOST_BRIDGE), which wins. Only that listener delivers to
	// the session; any other listener leaves it to its owner (session.ErrWrongMachine). Empty for
	// a session whose plugin predates this field; its own listener then falls back to
	// host.docker.internal.
	Host string `json:"host"`
}

func sessionEntryFromSubscribe(body subscribeBody, machineID string) session.SessionEntry {
	return session.SessionEntry{
		Port:           body.Port,
		MachineID:      machineID,
		Dir:            body.Dir,
		Title:          body.Title,
		Capabilities:   append([]string{}, body.Capabilities...),
		Driving:        body.Driving,
		SelfSubscribed: body.SelfSubscribed,
		Host:           body.Host,
	}
}
