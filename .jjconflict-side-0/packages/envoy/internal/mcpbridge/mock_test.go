package mcpbridge

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeMockServer writes a Go program that acts as a mock MCP server to disk
// and returns the path to the compiled binary.
func writeMockServer(dir string) string {
	src := `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type request struct {
	JSONRPC string          ` + "`json:\"jsonrpc\"`" + `
	ID      int             ` + "`json:\"id\"`" + `
	Method  string          ` + "`json:\"method\"`" + `
	Params  json.RawMessage ` + "`json:\"params,omitempty\"`" + `
}

type response struct {
	JSONRPC string          ` + "`json:\"jsonrpc\"`" + `
	ID      *int            ` + "`json:\"id,omitempty\"`" + `
	Result  json.RawMessage ` + "`json:\"result,omitempty\"`" + `
	Method  string          ` + "`json:\"method,omitempty\"`" + `
	Params  json.RawMessage ` + "`json:\"params,omitempty\"`" + `
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			resp := response{JSONRPC: "2.0", ID: &req.ID, Result: json.RawMessage(` + "`" + `{"protocolVersion":"2024-11-05","capabilities":{"resources":{"subscribe":true}},"serverInfo":{"name":"mock","version":"1.0.0"}}` + "`" + `)}
			data, _ := json.Marshal(resp)
			fmt.Fprintln(os.Stdout, string(data))

		case "notifications/initialized":
			// No response needed.

		case "resources/subscribe":
			resp := response{JSONRPC: "2.0", ID: &req.ID, Result: json.RawMessage(` + "`" + `{}` + "`" + `)}
			data, _ := json.Marshal(resp)
			fmt.Fprintln(os.Stdout, string(data))

			// After subscribing, immediately send a notification.
			var params struct{ URI string ` + "`json:\"uri\"`" + ` }
			json.Unmarshal(req.Params, &params)
			if params.URI != "" {
				notif := response{
					JSONRPC: "2.0",
					Method:  "notifications/resources/updated",
					Params:  json.RawMessage(fmt.Sprintf(` + "`" + `{"uri":"whatsapp://messages/15551234567/5551234567@s.whatsapp.net"}` + "`" + `)),
				}
				data, _ := json.Marshal(notif)
				fmt.Fprintln(os.Stdout, string(data))
			}

		case "resources/read":
			resp := response{JSONRPC: "2.0", ID: &req.ID, Result: json.RawMessage(` + "`" + `{"contents":[{"uri":"whatsapp://messages/15551234567/5551234567@s.whatsapp.net","text":"Hello from mock"}]}` + "`" + `)}
			data, _ := json.Marshal(resp)
			fmt.Fprintln(os.Stdout, string(data))
		}
	}
}
`
	srcPath := filepath.Join(dir, "mock_mcp_server.go")
	os.WriteFile(srcPath, []byte(src), 0644)
	return srcPath
}

// mockServerConfig returns a minimal config that uses a compiled binary as a mock MCP server.

// mockServerConfig returns a minimal config that uses a shell script as a mock MCP server.
// This approach avoids needing to compile a separate Go binary during tests.
func mockServerConfig(scriptPath string) string {
	cfg := map[string]any{
		"servers": []map[string]any{
			{
				"name":           "whatsapp",
				"transport":      "stdio",
				"command":        []string{scriptPath},
				"source":         "whatsapp",
				"topic_template": "notifications.whatsapp.{phone}.{jid}.message",
				"uri_pattern":    "whatsapp://messages/(?P<phone>[^/]+)/(?P<jid>.+)",
				"resources":      []string{"whatsapp://messages/new"},
			},
		},
	}
	data, _ := json.Marshal(cfg)
	return string(data)
}
