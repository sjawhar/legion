package mcpbridge

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

type ServerConfig struct {
	Name          string            `json:"name"`
	Transport     string            `json:"transport"`
	Command       []string          `json:"command"`
	URL           string            `json:"url"`
	Env           map[string]string `json:"env"`
	Resources     []string          `json:"resources"`
	Source        string            `json:"source"`
	TopicTemplate string            `json:"topic_template"`
	URIPattern    string            `json:"uri_pattern"`
	// PayloadRouting enables extracting topic variables from JSON payload
	// fields instead of the URI. When set, the bridge parses each resource
	// content as a JSON array and publishes one envelope per item.
	// Keys are topic template placeholders, values are JSON field names.
	// Example: {"phone": "chat", "jid": "chat_jid"}
	PayloadRouting map[string]string `json:"payload_routing,omitempty"`
	compiled *regexp.Regexp
}

type Config struct {
	Servers []ServerConfig `json:"servers"`
}

func (s *ServerConfig) CompiledPattern() *regexp.Regexp { return s.compiled }

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
	if s.Transport == "" {
		s.Transport = "stdio"
	}
	switch s.Transport {
	case "stdio":
		if s.URL != "" {
			return fmt.Errorf("url must not be set for stdio transport")
		}
		if len(s.Command) == 0 {
			return fmt.Errorf("command is required")
		}
	case "http":
		if len(s.Command) > 0 {
			return fmt.Errorf("command must not be set for http transport")
		}
		if s.URL == "" {
			return fmt.Errorf("url is required for http transport")
		}
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("url must be a valid http:// or https:// URL")
		}
	default:
		return fmt.Errorf("transport must be \"stdio\" or \"http\", got %q", s.Transport)
	}
	if strings.TrimSpace(s.Source) == "" {
		return fmt.Errorf("source is required")
	}
	if strings.TrimSpace(s.TopicTemplate) == "" {
		return fmt.Errorf("topic_template is required")
	}
	// When payload_routing is set, URI pattern is optional — the URI is just
	// a notification trigger, and topic variables come from the JSON payload.
	if len(s.PayloadRouting) > 0 {
		placeholders := extractPlaceholders(s.TopicTemplate)
		for _, ph := range placeholders {
			if _, ok := s.PayloadRouting[ph]; !ok {
				return fmt.Errorf("topic_template placeholder {%s} has no matching payload_routing key", ph)
			}
		}
	} else {
		if strings.TrimSpace(s.URIPattern) == "" {
			return fmt.Errorf("uri_pattern is required (or set payload_routing)")
		}
		re, err := regexp.Compile(s.URIPattern)
		if err != nil {
			return fmt.Errorf("uri_pattern: %w", err)
		}
		s.compiled = re
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
	}
	if s.Transport == "stdio" {
		if _, err := exec.LookPath(s.Command[0]); err != nil {
			return fmt.Errorf("command[0] %q not found: %w", s.Command[0], err)
		}
	}
	return nil
}

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

// RenderTopicFromPayload builds a topic string by extracting values from a
// JSON payload map using the PayloadRouting field mapping.
func (s *ServerConfig) RenderTopicFromPayload(payload map[string]interface{}) (string, error) {
	result := s.TopicTemplate
	for placeholder, jsonField := range s.PayloadRouting {
		val, ok := payload[jsonField]
		if !ok {
			return "", fmt.Errorf("payload missing field %q for placeholder {%s}", jsonField, placeholder)
		}
		result = strings.ReplaceAll(result, "{"+placeholder+"}", fmt.Sprint(val))
	}
	return result, nil
}
