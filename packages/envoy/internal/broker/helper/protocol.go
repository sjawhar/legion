// packages/envoy/internal/broker/helper/protocol.go

// Package helper is the host-session side of agent secrets (AGENTC-393): a per-user daemon
// that pins each registered process by pidfd, holds that session's P-256 key in memory, enrolls
// it with the secrets broker as kind "host", and signs proofs only for the session's
// descendants. The wire between the agent-secrets client and the helper is one JSON object per
// line each way on a unix stream socket, one request per connection.
package helper

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Request is the client's one line. Op is register, sign, sign-request, unregister, sessions,
// login, login-status, enroll-box or unenroll-box.
type Request struct {
	Op           string   `json:"op"`
	Method       string   `json:"method,omitempty"`        // sign: the HTTP method of the broker call
	URL          string   `json:"url,omitempty"`           // sign: its absolute URL
	Secrets      []string `json:"secrets,omitempty"`       // sign-request: the requested agent_secret names
	Reason       string   `json:"reason,omitempty"`        // sign-request: why they're needed
	WaitSeconds  int      `json:"wait_seconds,omitempty"`  // register: wait up to this long for the enrollment, unless the helper holds no launcher credential
	RuntimeID    string   `json:"runtime_id,omitempty"`    // enroll-box: the box's runtime id
	Thumbprint   string   `json:"thumbprint,omitempty"`    // enroll-box: the box key's thumbprint
	Kind         string   `json:"kind,omitempty"`          // enroll-box: always "box"
	SessionID    *string  `json:"session_id,omitempty"`    // enroll-box: optional registry session id
	EnrollmentID string   `json:"enrollment_id,omitempty"` // unenroll-box: which enrollment to delete
}

// Response is the helper's one line. Code and Error are set only when OK is false, except that
// a register reply carries the last broker error in Error while State is still "enrolling" (and
// Code NO_CREDENTIAL while the helper holds no launcher credential to enroll it with), and a
// login/login-status reply carries the confirmation code in Code while OK is true.
type Response struct {
	OK             bool          `json:"ok"`
	Code           string        `json:"code,omitempty"`
	Error          string        `json:"error,omitempty"`
	EnrollmentID   string        `json:"enrollment_id,omitempty"`
	RuntimeID      string        `json:"runtime_id,omitempty"`
	Operator       string        `json:"operator,omitempty"`
	State          string        `json:"state,omitempty"`           // register: enrolling | enrolled
	LoginState     string        `json:"login_state,omitempty"`     // login/login-status: the most recent login's pending|issued|denied|expired
	CredentialHeld bool          `json:"credential_held,omitempty"` // login-status: the helper holds a launcher credential, whatever the most recent login's state
	LoginRefused   bool          `json:"login_refused,omitempty"`   // login-status: the broker refused the credential the helper held, and no login has started since
	LeaseExpires   string        `json:"lease_expires,omitempty"`   // enroll-box: RFC3339Nano
	Proof          string        `json:"proof,omitempty"`
	RequestObject  string        `json:"request_object,omitempty"` // sign-request: the signed compact JWS
	Sessions       []SessionInfo `json:"sessions,omitempty"`
}

// SessionInfo is one registered session as `sessions` lists it: never a key.
type SessionInfo struct {
	PID          int    `json:"pid"`
	RuntimeID    string `json:"runtime_id"`
	EnrollmentID string `json:"enrollment_id"`
	State        string `json:"state"`
	RegisteredAt string `json:"registered_at"`
}

const (
	CodeNotASession = "NOT_A_SESSION"
	// CodeNotEnrolled answers sign or sign-request for a registered session that is still
	// enrolling: the helper holds a launcher credential, and its enroll loop has not succeeded yet.
	CodeNotEnrolled = "NOT_ENROLLED"
	// CodeNoCredential answers them instead while the helper holds no launcher credential, from
	// every restart until the operator logs the machine in: it enrolls no one, so the session has
	// no broker identity. A register reply for such a session carries it too, beside OK.
	CodeNoCredential   = "NO_CREDENTIAL"
	CodeBadRequest     = "BAD_REQUEST"
	CodeUnidentified   = "PEER_UNIDENTIFIED"
	CodeLoginFailed    = "LOGIN_FAILED"
	CodeEnrollFailed   = "ENROLL_FAILED"
	CodeUnenrollFailed = "UNENROLL_FAILED"
)

const maxLine = 64 << 10

// DefaultSocket is the helper's socket when AGENT_SECRETS_HELPER_SOCK is unset: under the
// user's runtime dir, which scripts/agentbox never mounts into a box. getenv is injectable so
// callers can test their own defaulting against a fake environment.
func DefaultSocket(getenv func(string) string) string {
	dir := getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	return filepath.Join(dir, "agent-secrets", "helper.sock")
}

// Call sends one request and reads one reply, all within timeout, the dial included (the dial
// alone also gives up after 2 s). A register with wait_seconds needs that much plus slack.
func Call(sock string, req Request, timeout time.Duration) (Response, error) {
	deadline := time.Now().Add(timeout)
	conn, err := (&net.Dialer{Timeout: 2 * time.Second, Deadline: deadline}).Dial("unix", sock)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	data, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return Response{}, err
	}
	line, err := bufio.NewReaderSize(conn, maxLine).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("helper reply is not one JSON object: %w", err)
	}
	return resp, nil
}
