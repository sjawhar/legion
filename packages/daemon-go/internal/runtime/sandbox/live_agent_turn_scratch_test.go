//go:build e2e

// Throwaway (never committed): a real Oh My Pi agent turn in a Sandbox pod of the worker image, on
// production, after gateway-token. The rig's pods run the image's own Oh My Pi in place of the sleep
// stub; one prompt goes over the worker stream, and the session file on the tree volume says who
// answered it.
package sandbox

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func init() {
	stubAgent = nil
	var kept []liveCheck
	for _, c := range liveChecks {
		kept = append(kept, c)
		if c.name == "gateway-token" {
			kept = append(kept, liveCheck{"agent-turn", (*liveRig).checkAgentTurn})
			break
		}
	}
	liveChecks = kept
}

const agentTurnPrompt = "Without calling any tool, reply with exactly four lines and nothing else. " +
	"Line 1: TOOLS: then the comma-separated names of every tool available to you. " +
	"Line 2: SKILLS: then the comma-separated names of every skill your instructions list. " +
	"Line 3: AGENTS: yes if your instructions include a repository AGENTS.md, else no. " +
	"Line 4: MODEL: then the model you are."

func (r *liveRig) checkAgentTurn() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	procs, err := r.exec(root, "sh", "-c", `for p in /proc/[0-9]*; do c=$(tr '\0' ' ' < $p/cmdline 2>/dev/null); case "$c" in "/opt/omp/bin/omp "*) echo "pid ${p#/proc/}: $c" | cut -c1-200; tr '\0' '\n' < $p/environ | grep -E '^(PI_CONFIG_FILES|PI_CONFIG_DIR|LEGION_MODEL_GATEWAY_URL)=';; esac; done; grep -A5 '^disabledProviders' /home/legion/.omp/profiles/legion/agent/config.yml >/dev/null; grep -E '^  - (claude|claude-plugins|codex|native)$' /home/legion/.omp/profiles/legion/agent/config.yml | tr '\n' ' '; echo; grep -A1 '^mcp:' /home/legion/.omp/profiles/legion/agent/config.yml | tr '\n' ' '`)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(procs, "\n") {
		note("operator", "agent: %s", line)
	}
	if !strings.Contains(procs, "--no-extensions --extension /opt/legion/pi-legion-envoy --mode rpc") {
		return fmt.Errorf("the agent's argv lacks --no-extensions --extension /opt/legion/pi-legion-envoy --mode rpc")
	}
	conn, ok := r.ln.Conn(root.token)
	if !ok {
		return fmt.Errorf("no worker stream connection for %s", root.token)
	}
	sent := time.Now()
	if err := conn.Prompt(r.ctx, "agent-turn-1", agentTurnPrompt); err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	var file string
	var assistant map[string]any
	err = r.poll(5*time.Minute, "the agent's session to record an answered turn", func() (bool, error) {
		out, err := r.exec(root, "sh", "-c", `f=$(ls -t `+ompSessionsDir+`/*/*.jsonl 2>/dev/null | head -1); [ -n "$f" ] && echo "$f" && cat "$f"`)
		if err != nil || out == "" {
			return false, nil
		}
		lines := strings.Split(out, "\n")
		file = lines[0]
		for _, line := range lines[1:] {
			var entry struct {
				Type    string         `json:"type"`
				Message map[string]any `json:"message"`
			}
			if json.Unmarshal([]byte(line), &entry) != nil || entry.Type != "message" || entry.Message["role"] != "assistant" {
				continue
			}
			if stop, _ := entry.Message["stopReason"].(string); stop == "" || stop == "toolUse" {
				continue
			}
			assistant = entry.Message
		}
		return assistant != nil, nil
	})
	if err != nil {
		return err
	}
	var answer string
	content, _ := assistant["content"].([]any)
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if text, _ := block["text"].(string); text != "" {
			answer += text
		}
	}
	usage, _ := json.Marshal(assistant["usage"])
	note("operator", "session file %s: answered after %s by %v/%v (%v), stopReason %v, usage %s",
		file, time.Since(sent).Round(time.Second), assistant["provider"], assistant["model"], assistant["api"], assistant["stopReason"], usage)
	for _, line := range strings.Split(strings.TrimSpace(answer), "\n") {
		note("operator", "answer: %s", line)
	}
	logs, _ := r.kubectl("logs", SandboxName(root.token), "-c", mainContainer)
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		note("operator", "container log: %.300s", line)
	}
	model := fmt.Sprint(assistant["provider"]) + "/" + fmt.Sprint(assistant["model"])
	if !strings.HasPrefix(model, "anthropic/") || !strings.HasSuffix(model, "-legion") {
		return fmt.Errorf("the turn was answered by %s, not an anthropic/*-legion alias", model)
	}
	if assistant["stopReason"] != "stop" {
		return fmt.Errorf("the turn ended %v: %v", assistant["stopReason"], assistant["errorMessage"])
	}
	return nil
}
