package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

type RegistryEntry struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Dir     string `json:"dir"`
	Session struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"session"`
}

type Deliverer struct {
	RegistryDir  string
	HostBridge   string
	OpencodeBin  string
	XDGConfig    string
	XDGData      string
	XDGCache     string
	RequestLimit time.Duration
}

func (d Deliverer) Deliver(item contracts.Envelope, interest store.Interest) error {
	entry, _ := d.Find(interest.SessionID)
	text := d.Text(item)
	if entry != nil && entry.Port > 0 {
		if err := d.prompt(entry.Port, interest.SessionID, text); err == nil {
			return nil
		}
	}
	return d.resume(interest.SessionID, interest.Dir, text)
}

func (d Deliverer) Find(sessionID string) (*RegistryEntry, error) {
	files, err := filepath.Glob(filepath.Join(d.RegistryDir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		buf, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var entry RegistryEntry
		if err := json.Unmarshal(buf, &entry); err != nil {
			continue
		}
		if entry.Session.ID == sessionID {
			return &entry, nil
		}
	}
	return nil, os.ErrNotExist
}

func (d Deliverer) Text(item contracts.Envelope) string {
	return fmt.Sprintf("[NOTIFICATION from %s]\n%s\n\nTopic: %s\nEvent ID: %s", item.Source, item.PayloadSummary, item.Topic, item.EventID)
}

func (d Deliverer) prompt(port int, sessionID string, text string) error {
	type promptBody struct {
		Parts []map[string]string `json:"parts"`
		Agent string              `json:"agent,omitempty"`
	}
	bodyData := promptBody{
		Parts: []map[string]string{{"type": "text", "text": text}},
	}
	if agent, err := d.lastAgent(port, sessionID); err == nil && agent != "" {
		bodyData.Agent = agent
	}
	body, err := json.Marshal(bodyData)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s:%d/session/%s/prompt_async", d.host(), port, sessionID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: d.timeout()}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("prompt_async returned %d", resp.StatusCode)
	}
	return nil
}

func (d Deliverer) lastAgent(port int, sessionID string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s:%d/session/%s/message?limit=20", d.host(), port, sessionID), nil)
	if err != nil {
		return "", err
	}
	client := http.Client{Timeout: d.timeout()}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("message list returned %d", resp.StatusCode)
	}
	var items []struct {
		Info struct {
			Role  string `json:"role"`
			Agent string `json:"agent"`
		} `json:"info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return "", err
	}
	var result string
	for _, item := range items {
		if item.Info.Role != "user" || item.Info.Agent == "" {
			continue
		}
		result = item.Info.Agent
	}
	return result, nil
}

func (d Deliverer) resume(sessionID string, dir string, text string) error {
	cmd := exec.Command(d.OpencodeBin, "run", "--session", sessionID, "--dir", dir, text)
	cmd.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+d.XDGConfig,
		"XDG_DATA_HOME="+d.XDGData,
		"XDG_CACHE_HOME="+d.XDGCache,
	)
	buf, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cold resume failed: %w: %s", err, strings.TrimSpace(string(buf)))
	}
	return nil
}

func (d Deliverer) host() string {
	if d.HostBridge != "" {
		return d.HostBridge
	}
	return "host.docker.internal"
}

func (d Deliverer) timeout() time.Duration {
	if d.RequestLimit > 0 {
		return d.RequestLimit
	}
	return 10 * time.Second
}
