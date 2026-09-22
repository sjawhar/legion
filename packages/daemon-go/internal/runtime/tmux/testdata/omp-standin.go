// Command omp-standin stands in for `omp --mode rpc` in the tmux runtime's real-tmux tests.
//
// It does, at boot, what the Legion plugin's Go-daemon client does: it reads its boot token from
// the file LEGION_BOOT_TOKEN_FILE names, registers with the daemon at LEGION_DAEMON_URL as the
// session it became (resuming the one --resume names, when given), and declares itself ready —
// with the request and response shapes of internal/claim, the ones the real daemon routes serve.
// Then it speaks OMP's RPC frames on stdin and stdout: negotiate_protocol, get_state, and a
// prompt, acknowledged and followed by one turn (agent_start, agent_end).
//
// It records what it was started with — its argv, the session, whether it resumed — in
// <cwd>/standin-<pid>.json once registered, for the test to read. A prompt whose message is
// "ignore SIGTERM" makes it ignore SIGTERM from then on: a process that will not end itself, so a
// stop has to fall back to killing its pane.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Report is what the stand-in writes once registered.
type Report struct {
	Pid          int      `json:"pid"`
	Argv         []string `json:"argv"`
	Mode         string   `json:"mode"`
	SystemPrompt []string `json:"systemPrompt"`
	Resumed      bool     `json:"resumed"`
	SessionID    string   `json:"sessionId"`
	SessionFile  string   `json:"sessionFile"`
	ClaimToken   string   `json:"claimToken"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "omp-standin:", err)
		os.Exit(1)
	}
}

func run() error {
	report := Report{Pid: os.Getpid(), Argv: os.Args}
	resume := ""
	for i := 1; i < len(os.Args); i++ {
		switch arg := os.Args[i]; {
		case strings.HasPrefix(arg, "--resume="):
			resume = strings.TrimPrefix(arg, "--resume=")
		case arg == "--mode" && i+1 < len(os.Args):
			i++
			report.Mode = os.Args[i]
		case arg == "--append-system-prompt" && i+1 < len(os.Args):
			i++
			report.SystemPrompt = append(report.SystemPrompt, os.Args[i])
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if resume != "" {
		id, err := os.ReadFile(resume)
		if err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		report.Resumed, report.SessionID, report.SessionFile = true, strings.TrimSpace(string(id)), resume
	} else {
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		report.SessionID = hex.EncodeToString(raw)
		report.SessionFile = filepath.Join(cwd, ".omp", "session-"+report.SessionID+".jsonl")
		if err := os.MkdirAll(filepath.Dir(report.SessionFile), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(report.SessionFile, []byte(report.SessionID+"\n"), 0o600); err != nil {
			return err
		}
	}

	token, err := os.ReadFile(os.Getenv("LEGION_BOOT_TOKEN_FILE"))
	if err != nil {
		return fmt.Errorf("read the boot token: %w", err)
	}
	daemon := os.Getenv("LEGION_DAEMON_URL")
	var registered claim.RegisterResponse
	if err := post(daemon+"/legion/v1/claims/register", claim.RegisterRequest{
		BootToken:      strings.TrimSpace(string(token)),
		SessionID:      report.SessionID,
		OmpSessionFile: report.SessionFile,
		AgentID:        "omp-standin",
		PluginContract: 1,
	}, &registered); err != nil {
		return err
	}
	report.ClaimToken = string(registered.ClaimToken)
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cwd, fmt.Sprintf("standin-%d.json", report.Pid)), encoded, 0o600); err != nil {
		return err
	}
	if err := post(daemon+"/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: registered.ClaimToken,
		SessionID:  report.SessionID,
		Secret:     registered.Secret,
		Generation: registered.Generation,
	}, nil); err != nil {
		return err
	}
	return serve(os.Stdin, os.Stdout)
}

// post sends body as JSON and decodes a 2xx answer into out; any other status ends the process,
// as the plugin ends it on a refused registration.
func post(url string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		text, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: %s: %s", url, resp.Status, text)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// serve answers OMP's RPC frames until stdin ends.
func serve(in io.Reader, out io.Writer) error {
	reader, writer := shimwire.NewReader(in), shimwire.NewWriter(out)
	for {
		line, err := reader.ReadLine()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			return err
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			err = writer.WriteFrame(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			err = writer.WriteFrame(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: json.RawMessage(`{"isStreaming":false}`)})
		case shimwire.Prompt:
			if f.Message == "ignore SIGTERM" {
				signal.Ignore(syscall.SIGTERM)
			}
			// OMP acknowledges before the turn starts, which is why the ack is not a delivery.
			if err = writer.WriteFrame(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true}); err == nil {
				if err = writer.WriteFrame(shimwire.AgentStart{}); err == nil {
					err = writer.WriteFrame(shimwire.AgentEnd{})
				}
			}
		default:
			err = writer.WriteLine([]byte(fmt.Sprintf(`{"type":"response","command":%q,"success":false,"error":"unsupported"}`, frame.FrameType())))
		}
		if err != nil {
			return err
		}
	}
}
