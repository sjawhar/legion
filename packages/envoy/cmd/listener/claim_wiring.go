package main

import "github.com/sjawhar/envoy/internal/session"

// subscribeBody is the POST /v1/interests/subscribe payload sent by each
// opencode process's envoy plugin, on subscribe and on every heartbeat.
type subscribeBody struct {
	SessionID string   `json:"session_id"`
	Dir       string   `json:"dir"`
	Topics    []string `json:"topics"`
	Port      int      `json:"port"`
	Title     string   `json:"title"`
	// Driving reports that this process is the one actually driving the session,
	// rather than a process that merely has it loaded from shared on-disk state.
	// Absent (older plugins) means "not driving", which is the safe default: a
	// claim without the flag cannot displace a live driving claim.
	Driving bool `json:"driving"`
}

func sessionEntryFromSubscribe(body subscribeBody, machineID string) session.SessionEntry {
	return session.SessionEntry{
		Port:      body.Port,
		MachineID: machineID,
		Dir:       body.Dir,
		Title:     body.Title,
		Driving:   body.Driving,
	}
}
