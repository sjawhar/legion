package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"time"
)

// runRecreate deletes the bucket's stream and adds it again with the config it had, so the
// listener's watcher finds a stream with a new creation time (its stream probe, or kvwatch Check),
// then fills it. With -fill-log it writes a fillLog: when the stream was deleted, created and
// filled, both streams' last sequences, and when each record of the fill was acknowledged.
func runRecreate(args []string) error {
	flags := flag.NewFlagSet("recreate", flag.ContinueOnError)
	url := flags.String("nats", "", "NATS URL")
	kind := flags.String("bucket", "", "sessions or interests")
	spec := flags.String("sets", "", "record sets the new bucket holds, name=count[:variant],...")
	role := flags.String("role", "", "role the first session of each set holds")
	markers := flags.Int("markers", 0, "delete markers to write after the records")
	fillLogPath := flags.String("fill-log", "", "file to write the fill's timeline to, as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	config, err := bucketConfig(*kind)
	if err != nil {
		return err
	}
	sets, err := parseSets(*spec)
	if err != nil {
		return err
	}
	conn, js, err := connect(*url)
	if err != nil {
		return err
	}
	defer conn.Close()
	stream := "KV_" + config.Bucket
	info, err := js.StreamInfo(stream)
	if err != nil {
		return fmt.Errorf("read %s: %w", stream, err)
	}
	started := time.Now()
	streamConfig := info.Config
	fill := fillLog{OldLastSeq: info.State.LastSeq, Deleted: time.Now().UnixNano()}
	if err := js.DeleteStream(stream); err != nil {
		return fmt.Errorf("delete %s: %w", stream, err)
	}
	created, err := js.AddStream(&streamConfig)
	fill.Created = time.Now().UnixNano()
	if err != nil {
		return fmt.Errorf("add %s again: %w", stream, err)
	}
	// nats-server removes a deleted stream's files in the background, and a create that comes
	// before that recovers the deleted stream's messages (internal/testnats says how), so a
	// recreate that is not empty is refused rather than filled.
	if created.State.Msgs != 0 {
		return fmt.Errorf("the recreated %s already holds %d messages of the deleted one", stream, created.State.Msgs)
	}
	var steps *[]fillStep
	if *fillLogPath != "" {
		steps = &fill.Steps
	}
	n, err := put(js, *kind, sets, *role, *markers, steps)
	if err != nil {
		return fmt.Errorf("fill %s: %w", config.Bucket, err)
	}
	fill.Filled = time.Now().UnixNano()
	if *fillLogPath != "" {
		filled, err := js.StreamInfo(stream)
		if err != nil {
			return fmt.Errorf("read %s after its fill: %w", stream, err)
		}
		fill.NewLastSeq = filled.State.LastSeq
		data, err := json.Marshal(fill)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*fillLogPath, data, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("recreated %s (created %s, was %s) with %d records and %d delete markers in %s\n", config.Bucket,
		created.Created.UTC().Format(time.RFC3339Nano), info.Created.UTC().Format(time.RFC3339Nano), n,
		*markers, time.Since(started).Round(time.Millisecond))
	return nil
}

// fillLog is what recreate writes with -fill-log, every time a Unix time in nanoseconds: a read
// that begins while the rig still fills the new bucket delivers the records stored by then, and the
// rest arrive as live updates, so a judge must know when each record was there to be read.
type fillLog struct {
	Deleted    int64      `json:"deleted"`
	Created    int64      `json:"created"`
	Filled     int64      `json:"filled"`
	OldLastSeq uint64     `json:"old_last_seq"`
	NewLastSeq uint64     `json:"new_last_seq"`
	Steps      []fillStep `json:"steps"`
}

// fillStep says the records of Set below index Upto were all acknowledged, so stored, by Acked.
type fillStep struct {
	Set   string `json:"set"`
	Upto  int    `json:"upto"`
	Acked int64  `json:"acked"`
}

// acked is when the record was acknowledged at the latest, or math.MaxInt64 for one the fill never
// wrote. Steps are in time order, so the first that covers the record is the earliest.
func (f fillLog) acked(set string, index int) int64 {
	for _, step := range f.Steps {
		if step.Set == set && index < step.Upto {
			return step.Acked
		}
	}
	return math.MaxInt64
}

func readFillLog(path string) (fillLog, error) {
	var f fillLog
	data, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	if f.Deleted == 0 || f.Filled == 0 {
		return f, fmt.Errorf("%s: no delete or fill time", path)
	}
	return f, nil
}
