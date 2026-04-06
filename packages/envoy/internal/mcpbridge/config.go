package mcpbridge

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// ServerConfig describes a single MCP server to connect to.
type ServerConfig struct {
	Name          string            `json:"name"`
	Transport     string            `json:"transport"`
	Command       []string          `json:"command"`
	Env           map[string]string `json:"env"`
	Resources     []string          `json:"resources"`
	Source        string            `json:"source"`
	TopicTemplate string            `json:"topic_template"`
	URIPattern    string            `json:"uri_pattern"`

	// compiled is the pre-compiled regex from URIPattern.
	compiled *regexp.Regexp
}

// Config is the top-level configuration for the MCP bridge.
type Config struct {
	Servers []ServerConfig `json:"servers"`
}

// CompiledPattern returns the pre-compiled regex for URI extraction.
func (s *ServerConfig) CompiledPattern() *regexp.Regexp {
	return s.compiled
}

// LoadConfig reads and validates the MCP bridge configuration from a JSON file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config read: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config parse: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if len(c.Servers) == 0 {
		return fmt.Errorf("config: no servers configured")
	}
	for i := range c.Servers {
		if err := c.Servers[i].validate(); err != nil {
			return fmt.Errorf("config: server[%d] (%s): %w", i, c.Servers[i].Name, err)
		}
	}
	return nil
}

func (s *ServerConfig) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if s.Transport != "stdio" {
		return fmt.Errorf("transport must be \"stdio\", got %q", s.Transport)
	}
	if len(s.Command) == 0 {
		return fmt.Errorf("command is required")
	}
	if strings.TrimSpace(s.Source) == "" {
		return fmt.Errorf("source is required")
	}
	if strings.TrimSpace(s.TopicTemplate) == "" {
		return fmt.Errorf("topic_template is required")
	}
	if strings.TrimSpace(s.URIPattern) == "" {
		return fmt.Errorf("uri_pattern is required")
	}

	// Compile URI pattern.
	re, err := regexp.Compile(s.URIPattern)
	if err != nil {
		return fmt.Errorf("uri_pattern: %w", err)
	}
	s.compiled = re

	// Verify every {placeholder} in topic_template has a matching named capture group.
	groups := make(map[string]bool)
	for _, name := range re.SubexpNames() {
		if name != "" {
			groups[name] = true
		}
	}
	placeholders := extractPlaceholders(s.TopicTemplate)
	if len(placeholders) == 0 {
		return fmt.Errorf("topic_template has no {placeholder} variables")
	}
	for _, ph := range placeholders {
		if !groups[ph] {
			return fmt.Errorf("topic_template placeholder {%s} has no matching named capture group in uri_pattern", ph)
		}
	}

	// Verify command[0] exists.
	if _, err := exec.LookPath(s.Command[0]); err != nil {
		return fmt.Errorf("command[0] %q not found: %w", s.Command[0], err)
	}

	return nil
}

// extractPlaceholders returns all {name} placeholder names from a template string.
func extractPlaceholders(tmpl string) []string {
	var out []string
	for {
		start := strings.Index(tmpl, "{")
		if start < 0 {
			break
		}
		end := strings.Index(tmpl[start:], "}")
		if end < 0 {
			break
		}
		name := tmpl[start+1 : start+end]
		if name != "" {
			out = append(out, name)
		}
		tmpl = tmpl[start+end+1:]
	}
	return out
}

// RenderTopic fills topic_template placeholders with values extracted from a URI.
func (s *ServerConfig) RenderTopic(uri string) (string, error) {
	match := s.compiled.FindStringSubmatch(uri)
	if match == nil {
		return "", fmt.Errorf("uri %q does not match pattern %s", uri, s.URIPattern)
	}
	result := s.TopicTemplate
	for i, name := range s.compiled.SubexpNames() {
		if name != "" && i < len(match) {
			result = strings.ReplaceAll(result, "{"+name+"}", match[i])
		}
	}
	return result, nil
}
