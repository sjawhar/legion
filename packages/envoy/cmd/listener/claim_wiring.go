package main

import (
	"strings"

	"github.com/sjawhar/envoy/internal/session"
)

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
	// address the session's owning listener reaches its serving port at, unless the session runs
	// on that listener's machine (it registered through it, or Host is that listener's machine
	// id) and the listener has a host bridge (ENVOY_HOST_BRIDGE), which then wins. The owner is
	// the listener the session's stored subscription names (the one
	// store.Registry.Match delivers on), else the one it registered through; only the owner
	// delivers to the session, and any other listener leaves inbox messages to it silently
	// (session.ErrWrongMachine). Empty for a session whose plugin predates
	// this field; its owning listener then falls back to host.docker.internal when the session
	// registered through it, else to the machine id of the listener it registered through.
	Host string `json:"host"`
	// Plugin and PluginVersion name the plugin the session registers through (its npm package
	// name) and that plugin's build, so the session list shows which build each session runs.
	// Empty for a session whose plugin predates the fields.
	Plugin        string `json:"plugin"`
	PluginVersion string `json:"plugin_version"`
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
		Plugin:         strings.TrimSpace(body.Plugin),
		PluginVersion:  strings.TrimSpace(body.PluginVersion),
	}
}
