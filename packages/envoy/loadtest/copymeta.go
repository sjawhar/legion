package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// readCopyMeta reads the -copy-meta file: a JSON object that describes the copy a foreign run
// measures (image digests, Postgres settings, node CPU model, namespace, run id), which meta.json
// records verbatim under copy. It refuses a file that is not one JSON object, naming the file.
func readCopyMeta(path string) (json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-copy-meta: %w", err)
	}
	b = bytes.TrimSpace(b)
	if !json.Valid(b) || len(b) == 0 || b[0] != '{' {
		return nil, fmt.Errorf("-copy-meta %s is not a JSON object", path)
	}
	return json.RawMessage(b), nil
}
