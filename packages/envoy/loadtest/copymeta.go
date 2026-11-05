package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
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
	if !json.Valid(b) || b[0] != '{' {
		return nil, fmt.Errorf("-copy-meta %s is not a JSON object", path)
	}
	return json.RawMessage(b), nil
}

// stepCopy is what loadgen curves reads of meta.json's copy, the object cluster.sh's
// write_copy_meta writes: the phase the cluster script ran the step in, whether the step sent
// reads alone, and the copy's Postgres tier, which together say where the step sits in the curves
// (curves.go, classify). The object's other fields (images, settings, nodes, pods) describe the
// copy and no command reads them.
type stepCopy struct {
	// Phase is empty in a run recorded before cluster.sh wrote it; curves then takes the step's
	// kind from its name, since such a copy names the series' Postgres tier for every step.
	Phase     string `json:"phase"`
	ReadsOnly bool   `json:"reads_only"`
	Postgres  struct {
		// CPU is the tier's vCPU count, as cluster.sh's state.json holds it: a string ("16").
		CPU string `json:"cpu"`
	} `json:"postgres"`
}

// phaseSoak is the cluster phase of the hour step (cluster.sh soak).
const phaseSoak = "soak"

// readStepCopy is meta.json's copy; ok is false without meta.json, without copy, and when copy
// is no object these fields can be read from (another harness's run).
func readStepCopy(m *metaFile) (c stepCopy, ok bool) {
	if m == nil || len(m.Copy) == 0 {
		return c, false
	}
	if json.Unmarshal(m.Copy, &c) != nil {
		return stepCopy{}, false
	}
	return c, true
}

// pgCPUs is the copy's Postgres vCPU count; ok is false when copy names none.
func (c stepCopy) pgCPUs() (float64, bool) {
	if c.Postgres.CPU == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(c.Postgres.CPU, 64)
	return n, err == nil && finite(n)
}
