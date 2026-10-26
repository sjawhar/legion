// Command refillrig is the Go half of scripts/e2e-refill.sh, the rig that proves on a real listener
// and a real NATS server that the listener keeps its copy of a bucket while it reads the bucket
// again, refuses its two list routes while it cannot stand behind that copy, and publishes no
// active-sessions sample while it refuses (INFRA-261). The script builds, starts and signals the
// processes; this command does what a shell cannot do quickly or exactly:
//
//   - seed writes session and interest records straight into their buckets, and register writes
//     them through the listener's own registration route (POST /v1/interests/subscribe);
//   - recreate deletes a bucket and creates it again with the same stream config, then fills it,
//     and can write when each record of the fill was acknowledged, so a judge knows what the new
//     bucket held at any moment of a fill that a read overlapped;
//   - poll asks one list route, or /metrics, as fast as it answers until it is signalled, and writes
//     one line per answer saying which seeded records it held;
//   - await waits for a listener log line, and can signal a process the moment it appears (and
//     continue a stopped one a fixed time later);
//   - judge reads the polls beside the listener's own log lines and prints the scenario's verdict:
//     PASS, FAIL <reason>, or INVALID <reason> when the run missed the window it was meant to
//     observe, which is never a pass.
//
// Every record belongs to a named set: session id ses_refill_<set>_<index>, title
// "refill <set> <index> v<variant>" and interest topic notifications.refill.<set>.v<variant>, so a
// poll tells which set a row is from and which version of it the listener answered. A set is
// written on the command line as name=count[:variant] (variant 1 by default), several separated by
// commas.
//
// It is a test fixture, not a product, and lives under internal/testnats beside natsd, the server
// the script runs.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// machineID is the machine every seeded record names; the script runs the listener under the same
// ENVOY_MACHINE_ID.
const machineID = "e2e-refill"

// The listener's refilling answer (cmd/listener writeListRefilling), which every refused list read
// must carry exactly.
const (
	refillingMessage    = "envoy: the agent list is refilling; try again in a moment"
	refillingCode       = "refilling"
	refillingRetryAfter = "5"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	commands := map[string]func([]string) error{
		"seed":     runSeed,
		"register": runRegister,
		"recreate": runRecreate,
		"poll":     runPoll,
		"await":    runAwait,
		"judge":    runJudge,
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "refillrig %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: refillrig seed|register|recreate|poll|await|judge [flags]; refillrig <command> -h lists a command's flags")
}

// recordSet is one named set of records: count records, written at variant.
type recordSet struct {
	name    string
	count   int
	variant int
}

func parseSets(spec string) ([]recordSet, error) {
	var sets []recordSet
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, rest, ok := strings.Cut(part, "=")
		if !ok || name == "" || strings.ContainsAny(name, "_.") {
			return nil, fmt.Errorf("set %q: want name=count[:variant], a name without _ or .", part)
		}
		countText, variantText, hasVariant := strings.Cut(rest, ":")
		count, err := strconv.Atoi(countText)
		if err != nil || count < 0 {
			return nil, fmt.Errorf("set %q: count %q is not a whole number", part, countText)
		}
		variant := 1
		if hasVariant {
			if variant, err = strconv.Atoi(variantText); err != nil || variant < 1 || variant > 9 {
				return nil, fmt.Errorf("set %q: variant %q is not 1-9", part, variantText)
			}
		}
		sets = append(sets, recordSet{name: name, count: count, variant: variant})
	}
	return sets, nil
}

func sessionID(set string, index int) string { return fmt.Sprintf("ses_refill_%s_%d", set, index) }

// parseSessionID names the set and index of a seeded session id.
func parseSessionID(id string) (string, int, bool) {
	rest, ok := strings.CutPrefix(id, "ses_refill_")
	if !ok {
		return "", 0, false
	}
	cut := strings.LastIndexByte(rest, '_')
	if cut <= 0 {
		return "", 0, false
	}
	index, err := strconv.Atoi(rest[cut+1:])
	if err != nil || index < 0 {
		return "", 0, false
	}
	return rest[:cut], index, true
}

func sessionTitle(set string, index, variant int) string {
	return fmt.Sprintf("refill %s %d v%d", set, index, variant)
}

func variantTopic(set string, variant int) string {
	return fmt.Sprintf("notifications.refill.%s.v%d", set, variant)
}

// topics are the topics a seeded session subscribes to: its set's variant topic, and the role topic
// for the first session of a set seeded with a role, which makes that session the role's holder in
// every list row (cmd/listener roleNames).
func topics(set recordSet, index int, role string) []string {
	out := []string{variantTopic(set.name, set.variant)}
	if role != "" && index == 0 {
		out = append(out, contracts.RoleTopicPrefix+role)
	}
	return out
}

func sessionRecord(set recordSet, index int, now int64) session.SessionEntry {
	return session.SessionEntry{
		MachineID:      machineID,
		Dir:            "/refill/" + set.name,
		Title:          sessionTitle(set.name, index, set.variant),
		UpdatedAt:      now,
		SelfSubscribed: true,
	}
}

func interestRecord(set recordSet, index int, now int64, role string) store.Interest {
	id := sessionID(set.name, index)
	return store.Interest{
		SessionID: id,
		MachineID: machineID,
		Dir:       "/refill/" + set.name,
		Topics:    append(topics(set, index, role), contracts.AgentSubject(id)),
		UpdatedAt: now,
	}
}

// bucketConfig is the config the listener creates each bucket with when it finds none
// (session.OpenSessionRegistry, store.Open), so a bucket seeded before the listener starts is the
// bucket the listener would have created.
func bucketConfig(kind string) (*nats.KeyValueConfig, error) {
	switch kind {
	case "sessions":
		return &nats.KeyValueConfig{Bucket: bus.SessionBucket, TTL: 5 * time.Minute, Replicas: 1, Storage: nats.FileStorage}, nil
	case "interests":
		return &nats.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: nats.FileStorage}, nil
	}
	return nil, fmt.Errorf("bucket %q: want sessions or interests", kind)
}

func connect(url string) (*nats.Conn, nats.JetStreamContext, error) {
	conn, err := nats.Connect(url, nats.Timeout(5*time.Second), nats.Name("refillrig"))
	if err != nil {
		return nil, nil, err
	}
	js, err := conn.JetStream(nats.PublishAsyncMaxPending(4096), nats.MaxWait(10*time.Second))
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, js, nil
}

// put writes every record of sets into the bucket of kind as KV puts, with markers delete markers
// on keys no record has spread evenly among them, all published asynchronously and each
// acknowledged before it returns. A read streams past every delete marker and lists none, so
// markers lengthen a read, and the time over which it delivers the records, without lengthening a
// list answer: that is how polls land inside a read. With steps, it appends one step per set each
// time records of that set are acknowledged: the records below Upto were all stored by Acked.
func put(js nats.JetStreamContext, kind string, sets []recordSet, role string, markers int, steps *[]fillStep) (int, error) {
	config, err := bucketConfig(kind)
	if err != nil {
		return 0, err
	}
	now := time.Now().UnixMilli()
	records := 0
	for _, set := range sets {
		records += set.count
	}
	var futures []nats.PubAckFuture
	// written counts the records of each set published so far, logged the ones a step names.
	written, logged := map[string]int{}, map[string]int{}
	// settle waits until every publish so far is acknowledged and fails on the first refused one.
	// nats.go refuses a publish while too many are outstanding rather than wait, so publish settles
	// every thousand.
	settle := func() error {
		select {
		case <-js.PublishAsyncComplete():
		case <-time.After(2 * time.Minute):
			return errors.New("puts unacknowledged after 2 minutes")
		}
		for _, future := range futures {
			select {
			case <-future.Ok():
			case err := <-future.Err():
				return err
			}
		}
		futures = futures[:0]
		if steps != nil {
			acked := time.Now().UnixNano()
			for _, set := range sets {
				if written[set.name] > logged[set.name] {
					*steps = append(*steps, fillStep{Set: set.name, Upto: written[set.name], Acked: acked})
					logged[set.name] = written[set.name]
				}
			}
		}
		return nil
	}
	publish := func(msg *nats.Msg) error {
		future, err := js.PublishMsgAsync(msg)
		if err != nil {
			return err
		}
		futures = append(futures, future)
		if len(futures) < 1000 {
			return nil
		}
		return settle()
	}
	published := 0
	// publishMarkers publishes the markers due once done records are out: markers*done/records.
	publishMarkers := func(done int) error {
		due := markers
		if records > 0 {
			due = markers * done / records
		}
		for ; published < due; published++ {
			marker := nats.NewMsg(fmt.Sprintf("$KV.%s.refill_marker_%d", config.Bucket, published))
			marker.Header.Set("KV-Operation", "DEL")
			if err := publish(marker); err != nil {
				return err
			}
		}
		return nil
	}
	done := 0
	for _, set := range sets {
		for index := range set.count {
			var record any = sessionRecord(set, index, now)
			if kind == "interests" {
				record = interestRecord(set, index, now, role)
			}
			value, err := json.Marshal(record)
			if err != nil {
				return 0, err
			}
			msg := nats.NewMsg("$KV." + config.Bucket + "." + sessionID(set.name, index))
			msg.Data = value
			if err := publish(msg); err != nil {
				return 0, err
			}
			done++
			written[set.name]++
			if err := publishMarkers(done); err != nil {
				return 0, err
			}
		}
	}
	if err := publishMarkers(records); err != nil {
		return 0, err
	}
	if err := settle(); err != nil {
		return 0, err
	}
	return records, nil
}

func kinds(spec string) ([]string, error) {
	var out []string
	for kind := range strings.SplitSeq(spec, ",") {
		if _, err := bucketConfig(kind); err != nil {
			return nil, err
		}
		out = append(out, kind)
	}
	return out, nil
}

func runSeed(args []string) error {
	flags := flag.NewFlagSet("seed", flag.ContinueOnError)
	url := flags.String("nats", "", "NATS URL")
	buckets := flags.String("buckets", "sessions,interests", "buckets to write: sessions, interests or both, comma-separated")
	spec := flags.String("sets", "", "record sets, name=count[:variant],...")
	role := flags.String("role", "", "role the first session of each set holds")
	create := flags.Bool("create", false, "create a missing bucket with the listener's config")
	ttl := flags.Duration("ttl", 0, "with -create, the session bucket's MaxAge in place of the listener's 5 minutes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *ttl > 0 && !*create {
		return errors.New("-ttl needs -create: an existing bucket keeps its MaxAge")
	}
	sets, err := parseSets(*spec)
	if err != nil {
		return err
	}
	list, err := kinds(*buckets)
	if err != nil {
		return err
	}
	conn, js, err := connect(*url)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, kind := range list {
		config, _ := bucketConfig(kind)
		if kind == "sessions" && *ttl > 0 {
			config.TTL = *ttl
		}
		if *create {
			_, err = bus.EnsureKeyValue(conn, js, config)
		} else {
			_, err = js.KeyValue(config.Bucket)
		}
		if err != nil {
			return fmt.Errorf("open %s: %w", config.Bucket, err)
		}
		started := time.Now()
		n, err := put(js, kind, sets, *role, 0, nil)
		if err != nil {
			return fmt.Errorf("seed %s: %w", config.Bucket, err)
		}
		fmt.Printf("seeded %d records into %s in %s\n", n, config.Bucket, time.Since(started).Round(time.Millisecond))
	}
	return nil
}

// runRegister registers every session of the sets through the listener's own route, as an agent's
// plugin does: one POST /v1/interests/subscribe each, which writes the interest and, for a
// self-subscribed session, the session record.
func runRegister(args []string) error {
	flags := flag.NewFlagSet("register", flag.ContinueOnError)
	url := flags.String("url", "", "listener URL")
	spec := flags.String("sets", "", "record sets, name=count[:variant],...")
	role := flags.String("role", "", "role the first session of each set holds")
	workers := flags.Int("concurrency", 8, "requests in flight")
	if err := flags.Parse(args); err != nil {
		return err
	}
	sets, err := parseSets(*spec)
	if err != nil {
		return err
	}
	type job struct {
		set   recordSet
		index int
	}
	jobs := make(chan job)
	errs := make(chan error, *workers)
	client := &http.Client{Timeout: 30 * time.Second}
	var wg sync.WaitGroup
	for range *workers {
		wg.Go(func() {
			for j := range jobs {
				body, _ := json.Marshal(map[string]any{
					"session_id":      sessionID(j.set.name, j.index),
					"dir":             "/refill/" + j.set.name,
					"title":           sessionTitle(j.set.name, j.index, j.set.variant),
					"topics":          topics(j.set, j.index, *role),
					"self_subscribed": true,
				})
				resp, err := client.Post(*url+"/v1/interests/subscribe", "application/json", bytes.NewReader(body))
				if err == nil {
					answer, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						err = fmt.Errorf("subscribe %s: %d %s", sessionID(j.set.name, j.index), resp.StatusCode, bytes.TrimSpace(answer))
					}
				}
				if err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		})
	}
	started := time.Now()
	n := 0
	for _, set := range sets {
		for index := range set.count {
			jobs <- job{set, index}
			n++
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
	}
	fmt.Printf("registered %d sessions through %s/v1/interests/subscribe in %s\n", n, *url, time.Since(started).Round(time.Millisecond))
	return nil
}

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

// observation is one answer a poll got, as one line of its output. Start and End are wall-clock
// Unix nanoseconds, comparable with the listener's log times. Sets holds, per seeded set the poll
// knows, one character per index run-length encoded ("c:n,c:n"): '.' when the answer did not
// list that record, its variant digit when it did, '?' when it listed it at no variant a seeded
// record has.
type observation struct {
	List       string            `json:"list"`
	Start      int64             `json:"start"`
	End        int64             `json:"end"`
	Status     int               `json:"status"`
	Transport  string            `json:"transport,omitempty"`
	RetryAfter string            `json:"retry_after,omitempty"`
	Error      string            `json:"error,omitempty"`
	Code       string            `json:"code,omitempty"`
	Body       string            `json:"body,omitempty"`
	Rows       int               `json:"rows"`
	Sets       map[string]string `json:"sets,omitempty"`
	Unknown    []string          `json:"unknown,omitempty"`
	Unknowns   int               `json:"unknowns,omitempty"`
	Sessions   *int64            `json:"envoy_active_sessions,omitempty"`
	Interests  *int64            `json:"envoy_active_interests,omitempty"`

	decoded map[string][]byte
}

var listPaths = map[string]string{"sessions": "/v1/sessions", "interests": "/v1/interests/", "metrics": "/metrics"}

func runPoll(args []string) error {
	flags := flag.NewFlagSet("poll", flag.ContinueOnError)
	url := flags.String("url", "", "listener URL")
	list := flags.String("list", "", "sessions, interests or metrics")
	spec := flags.String("sets", "", "the seeded sets an answer is read against, name=count,...")
	out := flags.String("out", "", "file the observations are written to, one JSON line each")
	interval := flags.Duration("interval", 0, "wait between two requests")
	timeout := flags.Duration("timeout", 30*time.Second, "one request's limit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if _, ok := listPaths[*list]; !ok {
		return fmt.Errorf("list %q: want sessions, interests or metrics", *list)
	}
	sets, err := parseSets(*spec)
	if err != nil {
		return err
	}
	file, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	client := &http.Client{Timeout: *timeout}
	for ctx.Err() == nil {
		o := observe(ctx, client, *url, *list, sets)
		if ctx.Err() != nil {
			break
		}
		if err := encoder.Encode(o); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		if *interval > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(*interval):
			}
		}
	}
	return writer.Flush()
}

func observe(ctx context.Context, client *http.Client, url, list string, sets []recordSet) observation {
	o := observation{List: list, Start: time.Now().UnixNano()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+listPaths[list], nil)
	if err != nil {
		o.Transport, o.End = err.Error(), time.Now().UnixNano()
		return o
	}
	resp, err := client.Do(req)
	if err != nil {
		o.Transport, o.End = err.Error(), time.Now().UnixNano()
		return o
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	o.End = time.Now().UnixNano()
	o.Status, o.RetryAfter = resp.StatusCode, resp.Header.Get("Retry-After")
	if err != nil {
		o.Status, o.Transport = 0, err.Error()
		return o
	}
	switch {
	case list == "metrics":
		o.Sessions, o.Interests = sample(body, "envoy_active_sessions"), sample(body, "envoy_active_interests")
	case resp.StatusCode == http.StatusOK:
		var rows []struct {
			SessionID string   `json:"session_id"`
			Title     string   `json:"title"`
			Topics    []string `json:"topics"`
		}
		if err := json.Unmarshal(body, &rows); err != nil {
			o.Transport = "undecodable 200: " + err.Error()
			return o
		}
		o.Rows = len(rows)
		index := map[string]recordSet{}
		chars := map[string][]byte{}
		for _, set := range sets {
			index[set.name] = set
			chars[set.name] = bytes.Repeat([]byte{'.'}, set.count)
		}
		for _, row := range rows {
			name, i, ok := parseSessionID(row.SessionID)
			set, known := index[name]
			if !ok || !known || i >= set.count || chars[name][i] != '.' {
				o.Unknowns++
				if len(o.Unknown) < 5 {
					o.Unknown = append(o.Unknown, row.SessionID)
				}
				continue
			}
			chars[name][i] = variantOf(list, name, i, row.Title, row.Topics)
		}
		o.Sets = map[string]string{}
		for name, c := range chars {
			o.Sets[name] = encodeRuns(c)
		}
	default:
		var answer struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if json.Unmarshal(body, &answer) == nil {
			o.Error, o.Code = answer.Error, answer.Code
		} else {
			o.Body = string(body[:min(len(body), 200)])
		}
	}
	return o
}

// variantOf reads which variant of its record a listed row is: a session row by its title, an
// interest row by its set's variant topic.
func variantOf(list, name string, index int, title string, topics []string) byte {
	if list == "sessions" {
		rest, ok := strings.CutPrefix(title, fmt.Sprintf("refill %s %d v", name, index))
		if ok && len(rest) == 1 && rest[0] >= '1' && rest[0] <= '9' {
			return rest[0]
		}
		return '?'
	}
	found := byte('?')
	prefix := "notifications.refill." + name + ".v"
	for _, topic := range topics {
		rest, ok := strings.CutPrefix(topic, prefix)
		if !ok {
			continue
		}
		if found != '?' || len(rest) != 1 || rest[0] < '1' || rest[0] > '9' {
			return '?'
		}
		found = rest[0]
	}
	return found
}

// sample is the value of the metric's sample in a /metrics answer, or nil when the answer has none.
func sample(body []byte, metric string) *int64 {
	for line := range strings.SplitSeq(string(body), "\n") {
		rest, ok := strings.CutPrefix(line, metric+" ")
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			continue
		}
		v := int64(math.Round(value))
		return &v
	}
	return nil
}

func encodeRuns(chars []byte) string {
	var b strings.Builder
	for i := 0; i < len(chars); {
		j := i
		for j < len(chars) && chars[j] == chars[i] {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%c:%d", chars[i], j-i)
		i = j
	}
	return b.String()
}

func decodeRuns(runs string) ([]byte, error) {
	var out []byte
	for run := range strings.SplitSeq(runs, ",") {
		if run == "" {
			continue
		}
		char, countText, ok := strings.Cut(run, ":")
		count, err := strconv.Atoi(countText)
		if !ok || len(char) != 1 || err != nil {
			return nil, fmt.Errorf("run %q", run)
		}
		out = append(out, bytes.Repeat([]byte(char), count)...)
	}
	return out, nil
}

// logLine is one line of the listener's log: its own JSON records, and the text lines the bus
// writes through the default logger (with a time to the second).
type logLine struct {
	at      int64
	msg     string
	outcome string
	entries int
	offset  int64
}

func parseLogLine(line []byte) (logLine, bool) {
	if len(line) > 0 && line[0] == '{' {
		var record struct {
			Time    string `json:"time"`
			Msg     string `json:"msg"`
			Outcome string `json:"outcome"`
			Entries int    `json:"entries"`
		}
		if json.Unmarshal(line, &record) != nil {
			return logLine{}, false
		}
		at, err := time.Parse(time.RFC3339Nano, record.Time)
		if err != nil {
			return logLine{}, false
		}
		return logLine{at: at.UnixNano(), msg: record.Msg, outcome: record.Outcome, entries: record.Entries}, true
	}
	const layout = "2006/01/02 15:04:05"
	if len(line) < len(layout)+1 {
		return logLine{}, false
	}
	at, err := time.ParseInLocation(layout, string(line[:len(layout)]), time.Local)
	if err != nil {
		return logLine{}, false
	}
	return logLine{at: at.UnixNano(), msg: strings.TrimSpace(string(line[len(layout):]))}, true
}

// readLog reads the log lines from offset on, each with the offset just past it.
func readLog(path string, offset int64) ([]logLine, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	var lines []logLine
	reader := bufio.NewReaderSize(file, 1<<20)
	position := offset
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 && raw[len(raw)-1] == '\n' {
			position += int64(len(raw))
			if line, ok := parseLogLine(bytes.TrimSpace(raw)); ok {
				line.offset = position
				lines = append(lines, line)
			}
		}
		if err == io.EOF {
			return lines, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

var signals = map[string]syscall.Signal{"STOP": syscall.SIGSTOP, "CONT": syscall.SIGCONT, "KILL": syscall.SIGKILL, "TERM": syscall.SIGTERM}

// matches is a repeatable -match flag: each value is text a line's message contains, optionally
// followed by @outcome, the line's outcome attribute.
type matches []string

func (m *matches) String() string        { return strings.Join(*m, " or ") }
func (m *matches) Set(text string) error { *m = append(*m, text); return nil }

// any reports whether line is one of the lines m names.
func (m matches) any(line logLine) bool {
	for _, match := range m {
		text, outcome, hasOutcome := strings.Cut(match, "@")
		if strings.Contains(line.msg, text) && (!hasOutcome || line.outcome == outcome) {
			return true
		}
	}
	return false
}

// runAwait waits for the first log line from offset on that one of the -match values names and
// whose time is after after-ns, signals pid at once when asked to, and prints the line's time, the
// signal's time (0 without one) and the offset past the line. With -cont-after it then continues
// the process it stopped that long after the stop, so a script blocked on that process's work
// (refillrig recreate filling a stopped NATS) still gets it back.
func runAwait(args []string) error {
	flags := flag.NewFlagSet("await", flag.ContinueOnError)
	path := flags.String("log", "", "listener log")
	offset := flags.Int64("offset", 0, "byte offset to read from")
	var match matches
	flags.Var(&match, "match", "text the line's message contains, optionally followed by @<outcome attribute>; repeatable, any one matches")
	after := flags.Int64("after-ns", 0, "skip lines logged at or before this Unix time in nanoseconds")
	timeout := flags.Duration("timeout", time.Minute, "how long to wait")
	signalName := flags.String("signal", "", "signal to send pid when the line appears: STOP, CONT, KILL or TERM")
	pid := flags.Int("pid", 0, "process to signal")
	contAfter := flags.Duration("cont-after", 0, "with -signal STOP, send pid SIGCONT this long after the stop")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(match) == 0 {
		return errors.New("no -match")
	}
	var sig syscall.Signal
	if *signalName != "" {
		var ok bool
		if sig, ok = signals[*signalName]; !ok || *pid <= 0 {
			return fmt.Errorf("signal %q to pid %d: want STOP, CONT, KILL or TERM and a pid", *signalName, *pid)
		}
	}
	if *contAfter > 0 && sig != syscall.SIGSTOP {
		return errors.New("-cont-after needs -signal STOP")
	}
	file, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(*offset, io.SeekStart); err != nil {
		return err
	}
	deadline := time.Now().Add(*timeout)
	position := *offset
	var pending []byte
	buffer := make([]byte, 1<<16)
	for {
		n, readErr := file.Read(buffer)
		pending = append(pending, buffer[:n]...)
		for {
			end := bytes.IndexByte(pending, '\n')
			if end < 0 {
				break
			}
			raw := pending[:end+1]
			pending = pending[end+1:]
			position += int64(len(raw))
			line, ok := parseLogLine(bytes.TrimSpace(raw))
			if !ok || !match.any(line) || line.at <= *after {
				continue
			}
			var signalled int64
			if sig != 0 {
				if err := syscall.Kill(*pid, sig); err != nil {
					return fmt.Errorf("signal %d: %w", *pid, err)
				}
				signalled = time.Now().UnixNano()
			}
			fmt.Printf("%d %d %d\n", line.at, signalled, position)
			if *contAfter > 0 {
				time.Sleep(*contAfter)
				if err := syscall.Kill(*pid, syscall.SIGCONT); err != nil {
					return fmt.Errorf("continue %d: %w", *pid, err)
				}
			}
			return nil
		}
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if n == 0 {
			if time.Now().After(deadline) {
				return fmt.Errorf("no line naming %s in %s within %s", match.String(), *path, *timeout)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
}

func readObservations(path string) ([]observation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []observation
	decoder := json.NewDecoder(bufio.NewReaderSize(file, 1<<20))
	for {
		var o observation
		if err := decoder.Decode(&o); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		o.decoded = map[string][]byte{}
		for name, runs := range o.Sets {
			chars, err := decodeRuns(runs)
			if err != nil {
				return nil, fmt.Errorf("%s: set %s: %w", path, name, err)
			}
			o.decoded[name] = chars
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}

// expectation is what a list must hold around one read of its bucket: before, the records the
// copy held when the read began; after, the records the bucket holds, which the copy holds exactly
// once the read has completed.
type expectation struct {
	before, after map[string]recordSet
}

func newExpectation(before, after string) (expectation, error) {
	b, err := parseSets(before)
	if err != nil {
		return expectation{}, err
	}
	a, err := parseSets(after)
	if err != nil {
		return expectation{}, err
	}
	e := expectation{before: map[string]recordSet{}, after: map[string]recordSet{}}
	for _, set := range b {
		e.before[set.name] = set
	}
	for _, set := range a {
		e.after[set.name] = set
	}
	return e, nil
}

func (e expectation) names() []string {
	var names []string
	for name := range e.before {
		names = append(names, name)
	}
	for name := range e.after {
		if _, ok := e.before[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func variantIn(sets map[string]recordSet, name string, index int) (byte, bool) {
	set, ok := sets[name]
	if !ok || index >= set.count {
		return 0, false
	}
	return byte('0' + set.variant), true
}

func (e expectation) total(sets map[string]recordSet) int {
	n := 0
	for _, set := range sets {
		n += set.count
	}
	return n
}

// The three periods of a read a 200 can fall in.
const (
	beforeCompletion = iota // it ended before the read completed: the copy held every record of before
	acrossCompletion        // it may have been answered before or after: the records both hold are there
	afterCompletion         // it began after the read completed: the copy holds exactly after
)

// counts is a count per set, printed as set×n.
type counts map[string]int

func (c counts) String() string {
	var parts []string
	for name, n := range c {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s×%d", name, n))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func (c counts) any() bool {
	for _, n := range c {
		if n > 0 {
			return true
		}
	}
	return false
}

// rowProblem names what is wrong with a 200 in period, or "" when nothing is.
func (e expectation) rowProblem(o observation, period int) string {
	missing, kept, stale, wrong := counts{}, counts{}, counts{}, counts{}
	for _, name := range e.names() {
		chars := o.decoded[name]
		size := max(e.before[name].count, e.after[name].count)
		for i := range size {
			c := byte('.')
			if i < len(chars) {
				c = chars[i]
			}
			bv, inBefore := variantIn(e.before, name, i)
			av, inAfter := variantIn(e.after, name, i)
			present := c != '.'
			if present && !(inBefore && c == bv) && !(inAfter && c == av) {
				wrong[name]++
				continue
			}
			switch period {
			case beforeCompletion:
				if inBefore && !present {
					missing[name]++
				}
			case acrossCompletion:
				if inBefore && inAfter && !present {
					missing[name]++
				}
			case afterCompletion:
				switch {
				case inAfter && !present:
					missing[name]++
				case inAfter && c != av:
					stale[name]++
				case !inAfter && present:
					kept[name]++
				}
			}
		}
	}
	var parts []string
	if missing.any() {
		parts = append(parts, "missing "+missing.String())
	}
	if kept.any() {
		parts = append(parts, "still listing "+kept.String())
	}
	if stale.any() {
		parts = append(parts, "old values of "+stale.String())
	}
	if wrong.any() {
		parts = append(parts, "values no seeded record has for "+wrong.String())
	}
	if o.Unknowns > 0 {
		parts = append(parts, fmt.Sprintf("%d rows the rig never seeded (%s)", o.Unknowns, strings.Join(o.Unknown, " ")))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("a 200 of %d rows %s", o.Rows, strings.Join(parts, "; "))
}

// regressed counts the records of after that o listed at their after value and next no longer
// does: a record the read delivered never goes back.
func (e expectation) regressed(o, next observation) counts {
	back := counts{}
	for name, set := range e.after {
		was, now := o.decoded[name], next.decoded[name]
		for i := range set.count {
			want := byte('0' + set.variant)
			if i < len(was) && was[i] == want && (i >= len(now) || now[i] != want) {
				back[name]++
			}
		}
	}
	return back
}

// partial reports whether o lists some record of after not yet at its after value.
func (e expectation) partial(o observation) bool {
	for name, set := range e.after {
		chars := o.decoded[name]
		for i := range set.count {
			if i >= len(chars) || chars[i] != byte('0'+set.variant) {
				return true
			}
		}
	}
	return false
}

// verdict is a judge's finding: problems make it FAIL, an invalid reason INVALID, neither PASS.
type verdict struct {
	invalid  string
	problems []string
	notes    []string
}

func (v *verdict) fail(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *verdict) print() {
	switch {
	case len(v.problems) > 0:
		more := ""
		if len(v.problems) > 1 {
			more = fmt.Sprintf(" (and %d more: %s)", len(v.problems)-1, strings.Join(v.problems[1:min(len(v.problems), 4)], "; "))
		}
		fmt.Printf("FAIL %s%s\n", v.problems[0], more)
	case v.invalid != "":
		fmt.Printf("INVALID %s\n", v.invalid)
	default:
		fmt.Printf("PASS %s\n", strings.Join(v.notes, "; "))
	}
}

func ms(d int64) string { return fmt.Sprintf("%dms", d/int64(time.Millisecond)) }

// at names o's time relative to origin.
func at(o observation, origin int64) string {
	return fmt.Sprintf("the poll at %+.3fs", float64(o.Start-origin)/float64(time.Second))
}

func answer(o observation) string {
	if o.Status == 0 {
		return "no answer (" + o.Transport + ")"
	}
	text := fmt.Sprintf("%d", o.Status)
	if o.Error != "" || o.Code != "" {
		text += fmt.Sprintf(" %q code=%q", o.Error, o.Code)
	}
	if o.Body != "" {
		text += " " + o.Body
	}
	if o.RetryAfter != "" {
		text += " Retry-After=" + o.RetryAfter
	}
	return text
}

func refilling(o observation) bool {
	return o.Status == http.StatusServiceUnavailable && o.Error == refillingMessage && o.Code == refillingCode && o.RetryAfter == refillingRetryAfter
}

// complete reports whether o is a 200 listing exactly the records of sets.
func complete(o observation, sets map[string]recordSet) bool {
	if o.Status != http.StatusOK || o.Transport != "" {
		return false
	}
	return expectation{before: sets, after: sets}.rowProblem(o, afterCompletion) == "" && o.Rows == expectation{}.total(sets)
}

func runJudge(args []string) error {
	if len(args) == 0 {
		return errors.New("judge reread|interrupt|boot|outage|recreate-live")
	}
	judges := map[string]func([]string) (verdict, error){
		"reread":        judgeReread,
		"interrupt":     judgeInterrupt,
		"boot":          judgeBoot,
		"outage":        judgeOutage,
		"recreate-live": judgeRecreateLive,
	}
	judge, ok := judges[args[0]]
	if !ok {
		return fmt.Errorf("judge %q: want reread, interrupt, boot, outage or recreate-live", args[0])
	}
	v, err := judge(args[1:])
	if err != nil {
		return err
	}
	v.print()
	return nil
}

func firstLine(lines []logLine, after int64, match func(logLine) bool) (logLine, bool) {
	for _, line := range lines {
		if line.at >= after && match(line) {
			return line, true
		}
	}
	return logLine{}, false
}

// The kvwatch lines about a recreated bucket. The watcher's stream probe (or the gate on a record,
// or the check before a read completes) logs foundLine when it finds the stream replaced under it,
// and every watch that installs onto a stream other than the one read before logs installLine as
// the read of the new bucket begins: after foundLine for a recreate the watcher found itself, alone
// for one a Rewatch read first (the self-health rebuild after Check, or the reconnect hook).
func foundLine(registry string) func(logLine) bool {
	return func(l logLine) bool {
		return strings.HasPrefix(l.msg, registry+" bucket was recreated under its watcher")
	}
}

func installLine(registry string) func(logLine) bool {
	return func(l logLine) bool { return strings.HasPrefix(l.msg, registry+" bucket was recreated;") }
}

// installText is installLine's text, for the messages that name it.
func installText(registry string) string { return registry + " bucket was recreated;" }

func warmUpLine(registry, outcome string) func(logLine) bool {
	return func(l logLine) bool {
		return l.msg == registry+" cache warm-up" && (outcome == "" || l.outcome == outcome)
	}
}

// judgeList applies an expectation to every answer of one list around a read that completed at
// completed (and, after a failed read, at last, when the sweep may come from a later read):
// every answer is a 200, nothing of before is missing until the read completed, the records both
// hold never are, nothing the read delivered goes back, and once settle has passed since the last
// completion the list is exactly after.
func judgeList(v *verdict, label string, obs []observation, e expectation, origin, completed, last int64, settle time.Duration) (after int) {
	var previous *observation
	for i := range obs {
		o := obs[i]
		if o.Status != http.StatusOK || o.Transport != "" {
			v.fail("%s: %s answered %s", label, at(o, origin), answer(o))
			continue
		}
		period := acrossCompletion
		switch {
		case o.End < completed:
			period = beforeCompletion
		case o.Start > last+int64(settle):
			period = afterCompletion
			after++
		}
		if problem := e.rowProblem(o, period); problem != "" {
			v.fail("%s: %s got %s", label, at(o, origin), problem)
		}
		if previous != nil {
			if back := e.regressed(*previous, o); back.any() {
				v.fail("%s: %s no longer listed at their new value records an earlier poll had: %s", label, at(o, origin), back)
			}
		}
		previous = &obs[i]
	}
	return after
}

// judgeReread judges one recreate of a bucket read again by the listener (proof P1): the recreated
// list's answers against before and after, and the other list's answers against its records, which
// nothing changed. The script stops the listener while it recreates and fills the bucket, so the
// read the listener begins once it runs again (its watcher's probe finds the recreate) reads the
// whole bucket; one that delivered less began before the fill was done, and the run is INVALID.
func judgeReread(args []string) (verdict, error) {
	flags := flag.NewFlagSet("judge reread", flag.ContinueOnError)
	path := flags.String("log", "", "listener log")
	offset := flags.Int64("offset", 0, "log offset the phase began at")
	registry := flags.String("registry", "", "the cache's log name: session registry or interest registry")
	obsPath := flags.String("obs", "", "the recreated bucket's list polls")
	before := flags.String("before", "", "sets the copy held before the read")
	after := flags.String("after", "", "sets the recreated bucket holds")
	steadyPath := flags.String("steady-obs", "", "the other list's polls")
	steady := flags.String("steady-sets", "", "sets the other list holds throughout")
	settle := flags.Duration("settle", time.Second, "time after the completion line by which the sweep is done")
	var v verdict
	if err := flags.Parse(args); err != nil {
		return v, err
	}
	e, err := newExpectation(*before, *after)
	if err != nil {
		return v, err
	}
	lines, err := readLog(*path, *offset)
	if err != nil {
		return v, err
	}
	obs, err := readObservations(*obsPath)
	if err != nil {
		return v, err
	}
	begin, ok := firstLine(lines, 0, installLine(*registry))
	if !ok {
		v.invalid = fmt.Sprintf("the log has no %q line after the recreate: the listener never read the new bucket", installText(*registry))
		return v, nil
	}
	end, ok := firstLine(lines, begin.at, warmUpLine(*registry, "completed"))
	if !ok {
		v.invalid = fmt.Sprintf("the log has no completed %q line after the recreate", *registry+" cache warm-up")
		return v, nil
	}
	if want := e.total(e.after); end.entries != want {
		v.invalid = fmt.Sprintf("the read delivered %d records and the recreated bucket holds %d: it began before the rig had filled the bucket", end.entries, want)
		return v, nil
	}
	if incomplete, ok := firstLine(lines, begin.at, warmUpLine(*registry, "timed out")); ok && incomplete.at <= end.at {
		v.invalid = "the read timed out before the read that completed: not the one uninterrupted read this scenario observes"
		return v, nil
	}
	judgeList(&v, *registry, obs, e, begin.at, end.at, end.at, *settle)
	inside, partial := 0, 0
	for _, o := range obs {
		if o.Status == http.StatusOK && o.Start >= begin.at+int64(time.Millisecond) && o.End <= end.at {
			inside++
			if e.partial(o) {
				partial++
			}
		}
	}
	if *steadyPath != "" {
		steadyObs, err := readObservations(*steadyPath)
		if err != nil {
			return v, err
		}
		sets, err := newExpectation(*steady, *steady)
		if err != nil {
			return v, err
		}
		judgeList(&v, "the other list", steadyObs, sets, begin.at, end.at, end.at, 0)
		v.notes = append(v.notes, fmt.Sprintf("the other list held its %d records in all %d polls", sets.total(sets.after), len(steadyObs)))
	}
	if len(v.problems) == 0 && inside == 0 {
		v.invalid = fmt.Sprintf("no poll fell inside the %s read (%d polls in all)", ms(end.at-begin.at), len(obs))
	}
	found := "no line says the watcher found the recreate itself"
	if line, ok := firstLine(lines, 0, foundLine(*registry)); ok && line.at <= begin.at {
		found = fmt.Sprintf("the watcher's probe found the recreate %s before the read began", ms(begin.at-line.at))
	}
	v.notes = append([]string{fmt.Sprintf("%d of %d polls fell inside the %s read of %d records, %d of them with the new records partly delivered; every record of the old copy held until the read completed, exactly the bucket's after it; %s",
		inside, len(obs), ms(end.at-begin.at), end.entries, partial, found)}, v.notes...)
	return v, nil
}

// judgeInterrupt judges a read of a recreated bucket the rig interrupted (proof P2): the read must
// have been under way at the interruption, end incomplete, remove nothing, and be followed by a
// complete read the listener began itself, which then sweeps.
func judgeInterrupt(args []string) (verdict, error) {
	flags := flag.NewFlagSet("judge interrupt", flag.ContinueOnError)
	path := flags.String("log", "", "listener log")
	offset := flags.Int64("offset", 0, "log offset the phase began at")
	registry := flags.String("registry", "session registry", "the cache's log name")
	obsPath := flags.String("obs", "", "the list's polls")
	before := flags.String("before", "", "sets the copy held before the read")
	after := flags.String("after", "", "sets the recreated bucket holds")
	interrupted := flags.Int64("interrupted-ns", 0, "when the rig interrupted NATS")
	mode := flags.String("mode", "stop", "stop (SIGSTOP past the idle timer) or restart (kill and start again)")
	settle := flags.Duration("settle", 2*time.Second, "time after the last completion line by which the sweep is done")
	var v verdict
	if err := flags.Parse(args); err != nil {
		return v, err
	}
	e, err := newExpectation(*before, *after)
	if err != nil {
		return v, err
	}
	lines, err := readLog(*path, *offset)
	if err != nil {
		return v, err
	}
	obs, err := readObservations(*obsPath)
	if err != nil {
		return v, err
	}
	begin, ok := firstLine(lines, 0, installLine(*registry))
	if !ok {
		v.invalid = fmt.Sprintf("the log has no %q line: the rig never saw the read begin", installText(*registry))
		return v, nil
	}
	first, ok := firstLine(lines, begin.at, warmUpLine(*registry, ""))
	if ok && first.outcome == "completed" && first.at <= *interrupted {
		v.invalid = fmt.Sprintf("the scan completed (%d records in %s) before the interruption at +%s", first.entries, ms(first.at-begin.at), ms(*interrupted-begin.at))
		return v, nil
	}
	var outcomes []string
	if line, ok := firstLine(lines, begin.at, warmUpLine(*registry, "timed out")); ok {
		outcomes = append(outcomes, fmt.Sprintf("the scan timed out at +%s (%d records delivered)", ms(line.at-begin.at), line.entries))
	}
	if line, ok := firstLine(lines, begin.at, func(l logLine) bool { return l.msg == *registry+" watcher stopped" }); ok {
		outcomes = append(outcomes, fmt.Sprintf("the watcher ended before its scan did at +%s", ms(line.at-begin.at)))
	}
	// The bus logs through the default logger, whose time is to the second.
	if line, ok := firstLine(lines, begin.at-int64(time.Second), func(l logLine) bool { return strings.Contains(l.msg, "envoy nats disconnected") }); ok {
		outcomes = append(outcomes, fmt.Sprintf("the connection was lost at +%ds (the disconnect hook stopped the copy following; the reconnect hook read again)", (line.at-begin.at)/int64(time.Second)))
	}
	if line, ok := firstLine(lines, begin.at, func(l logLine) bool {
		return l.msg == *registry+" re-read of a timed-out scan failed; it is tried again"
	}); ok {
		outcomes = append(outcomes, fmt.Sprintf("a re-read failed at +%s and was tried again", ms(line.at-begin.at)))
	}
	completed, ok := firstLine(lines, *interrupted, warmUpLine(*registry, "completed"))
	if !ok {
		// No read completed after the interruption, so every poll of the run must still hold the
		// whole old copy: judge that too, so the verdict also says what the listener removed.
		judgeList(&v, *registry, obs, e, begin.at, math.MaxInt64, math.MaxInt64, 0)
		seen := strings.Join(outcomes, "; ")
		if seen == "" {
			seen = "the log shows no scan end, watcher end or disconnect either"
		}
		v.fail("no complete read of the bucket followed the interrupted one (%s): the listener did not read it again by itself", seen)
		return v, nil
	}
	if len(outcomes) == 0 {
		v.invalid = fmt.Sprintf("the %s interruption left the read whole: no timed-out scan, ended watcher or disconnect in the log before the read completed at +%s", *mode, ms(completed.at-begin.at))
		return v, nil
	}
	last := completed
	for _, line := range lines {
		if line.at > completed.at && warmUpLine(*registry, "completed")(line) {
			last = line
		}
	}
	afterSweep := judgeList(&v, *registry, obs, e, begin.at, completed.at, last.at, *settle)
	during := 0
	for _, o := range obs {
		if o.Status == http.StatusOK && o.Start >= *interrupted && o.End <= completed.at {
			during++
		}
	}
	if len(v.problems) == 0 {
		switch {
		case during == 0:
			v.invalid = "no poll fell between the interruption and the complete read"
		case afterSweep == 0:
			v.invalid = "no poll came after the sweep"
		}
	}
	v.notes = append(v.notes,
		fmt.Sprintf("interrupted by %s at +%s into the read: %s", *mode, ms(*interrupted-begin.at), strings.Join(outcomes, "; ")),
		fmt.Sprintf("%d polls during the incomplete read each held every record of the old copy", during),
		fmt.Sprintf("a complete read of %d records followed at +%s with no action of the rig's, and %d polls after it listed exactly the bucket", completed.entries, ms(completed.at-begin.at), afterSweep))
	return v, nil
}

// judgeRecreateLive judges a bucket recreated under a live watcher well before the self-health
// monitor's next tick, so that tick's Check cannot be what reads it: the watcher must find the
// recreate itself (its stream probe) and complete its read of the new bucket before that tick, with
// no self-health failure or rebuild on the way. Every 200 must list each record of the old copy
// until that read completes, the renewed session's included, whose old record would have expired
// inside the re-read: after the finding, which stops the copy following, and before the read's
// completion. A 200 answered while the listener still claimed its copy follows the bucket
// (before any line says it stopped) must not list records only the new bucket has while it lacks
// others the new bucket held: that is a consumer nats.go moved onto the new stream, resuming past
// the records its old sequence covers. After the read a 200 lists every record the read could have
// delivered, and once the read and the fill are both done, exactly the new bucket.
func judgeRecreateLive(args []string) (verdict, error) {
	flags := flag.NewFlagSet("judge recreate-live", flag.ContinueOnError)
	path := flags.String("log", "", "listener log")
	offset := flags.Int64("offset", 0, "log offset the shape began at")
	registry := flags.String("registry", "session registry", "the cache's log name")
	obsPath := flags.String("obs", "", "the list's polls")
	before := flags.String("before", "", "sets the old bucket held")
	after := flags.String("after", "", "sets the new bucket holds")
	fillPath := flags.String("fill-log", "", "the fill log recreate wrote")
	ready := flags.Int64("ready-ns", 0, "the listener's ready line, from which its self-health monitor ticks")
	interval := flags.Duration("tick", 30*time.Second, "the self-health monitor's interval")
	renewed := flags.String("renewed", "", "the set whose old record would expire inside the read and which the new bucket renews")
	renewedFrom := flags.Int64("renewed-from-ns", 0, "when the rig began writing the renewed set's old record")
	renewedTo := flags.Int64("renewed-to-ns", 0, "when that write was acknowledged")
	ttl := flags.Duration("ttl", 0, "the session bucket's MaxAge")
	shape := flags.String("shape", "", "shorter or longer: the old stream against the new one")
	settle := flags.Duration("settle", time.Second, "how long a record stored in a followed bucket may take to be listed")
	margin := flags.Duration("scan-margin", 250*time.Millisecond, "how long before the read's install line a record must have been stored to be in its scan")
	var v verdict
	if err := flags.Parse(args); err != nil {
		return v, err
	}
	if *ready == 0 || *ttl <= 0 || *renewed == "" || *renewedFrom == 0 || *renewedTo == 0 {
		return v, errors.New("-ready-ns, -ttl, -renewed, -renewed-from-ns and -renewed-to-ns are required")
	}
	e, err := newExpectation(*before, *after)
	if err != nil {
		return v, err
	}
	fill, err := readFillLog(*fillPath)
	if err != nil {
		return v, err
	}
	lines, err := readLog(*path, *offset)
	if err != nil {
		return v, err
	}
	obs, err := readObservations(*obsPath)
	if err != nil {
		return v, err
	}
	invalid := func(format string, args ...any) {
		if v.invalid == "" {
			v.invalid = fmt.Sprintf(format, args...)
		}
	}
	origin := fill.Deleted
	since := func(t int64) string { return fmt.Sprintf("%+.3fs", float64(t-origin)/float64(time.Second)) }
	tick := *ready + ((origin-*ready)/int64(*interval)+1)*int64(*interval)
	if (*shape == "shorter" && fill.OldLastSeq >= fill.NewLastSeq) || (*shape == "longer" && fill.OldLastSeq <= fill.NewLastSeq) {
		invalid("the old stream ended at sequence %d and the new one at %d: not the %s old stream this shape needs", fill.OldLastSeq, fill.NewLastSeq, *shape)
	}

	found, hasFound := firstLine(lines, origin, foundLine(*registry))
	install, hasInstall := firstLine(lines, origin, installLine(*registry))
	var completed logLine
	hasCompleted := false
	readEnd := int64(math.MaxInt64)
	if hasInstall {
		if completed, hasCompleted = firstLine(lines, install.at, warmUpLine(*registry, "completed")); hasCompleted {
			readEnd = completed.at
		}
	}
	health, hasHealth := firstLine(lines, origin, func(l logLine) bool {
		return l.msg == "self-health probe failed" || l.msg == "self-health rebuild started"
	})
	hasHealth = hasHealth && health.at <= readEnd
	healthText := ""
	if hasHealth {
		healthText = fmt.Sprintf(" (the self-health monitor's %q at %s)", health.msg, since(health.at))
	}
	// stopped is when a line first said the copy no longer follows its bucket: until then the
	// listener claims its copy is the bucket.
	stopped := int64(math.MaxInt64)
	if line, ok := firstLine(lines, origin, func(l logLine) bool {
		return foundLine(*registry)(l) || installLine(*registry)(l) ||
			strings.HasPrefix(l.msg, *registry+" bucket was deleted under its watcher") ||
			l.msg == *registry+" watcher stopped" || strings.Contains(l.msg, "envoy nats disconnected")
	}); ok {
		stopped = line.at
	}

	switch {
	case !hasFound:
		v.fail("the watcher never found the bucket recreated under it: no %q line", *registry+" bucket was recreated under its watcher")
	case found.at >= tick:
		v.fail("the watcher found the recreated bucket only at %s, after the self-health tick at %s", since(found.at), since(tick))
	}
	switch {
	case !hasInstall:
		v.fail("no read of the new bucket began: no %q line", installText(*registry))
	case install.at >= tick:
		v.fail("the read of the new bucket began only at %s, after the self-health tick at %s%s", since(install.at), since(tick), healthText)
	case !hasCompleted:
		v.fail("the read of the new bucket that began at %s never completed", since(install.at))
	case completed.at >= tick:
		invalid("the self-health tick at %s fell inside the read of the new bucket, from %s to %s: the run missed its window", since(tick), since(install.at), since(completed.at))
	case hasHealth:
		v.fail("the self-health monitor took part before the read of the new bucket completed: %q at %s", health.msg, since(health.at))
	}

	// The copy must keep the renewed session's old record past its expiry only when it had stopped
	// following its bucket by then: a copy still following prunes it, as the old bucket would have.
	expiresFrom, expiresBy := *renewedFrom+int64(*ttl), *renewedTo+int64(*ttl)
	renewedKept := stopped < expiresFrom
	if !renewedKept {
		invalid("the copy still claimed to follow its bucket when %s's old record could expire at %s: the run missed its window", *renewed, since(expiresFrom))
	}

	settleNs := int64(*settle)
	scanBy := int64(math.MinInt64)
	if hasInstall {
		scanBy = install.at - int64(*margin)
	}
	whole := int64(math.MaxInt64)
	if hasCompleted {
		whole = max(readEnd, fill.Filled) + settleNs
	}
	var wrong, claimed pollRun
	inside, partial, keptRenewed, exact := 0, 0, 0, 0
	var previous *observation
	for i := range obs {
		o := obs[i]
		if o.Status != http.StatusOK || o.Transport != "" {
			v.fail("GET /v1/sessions: %s answered %s", at(o, origin), answer(o))
			continue
		}
		r := newLiveRows()
		for _, name := range e.names() {
			chars := o.decoded[name]
			for index := range max(e.before[name].count, e.after[name].count) {
				c := charAt(chars, index)
				bv, inBefore := variantIn(e.before, name, index)
				av, inAfter := variantIn(e.after, name, index)
				present := c != '.'
				if present && !(inBefore && c == bv) && !(inAfter && c == av) {
					r.wrong[name]++
					continue
				}
				acked := int64(math.MaxInt64)
				if inAfter {
					acked = fill.acked(name, index)
				}
				// held: the new bucket held the record long enough before o for a copy following
				// it to list it; scanned: it held it when the read of the new bucket began.
				held, scanned := acked < o.Start-settleNs, acked < scanBy
				if present && inAfter && c == av && !(inBefore && bv == av) {
					r.fresh[name]++
				}
				if held && !(present && c == av) {
					r.lacking[name]++
				}
				oldCopy := inBefore && (name != *renewed || renewedKept || o.End < expiresFrom)
				// required: o must list the record. Until the read completed the copy kept every
				// record of the old copy; a poll that may have been answered on either side of the
				// sweep holds the ones the read scanned; after it, every record the read scanned or
				// the bucket held long enough to arrive as a live update.
				var required bool
				switch {
				case o.End < readEnd:
					required = oldCopy
				case o.Start <= readEnd:
					required = oldCopy && scanned
				default:
					required = scanned || held
					if o.Start > readEnd+settleNs {
						if present && inAfter && c != av {
							r.stale[name]++
						}
						if present && !inAfter {
							r.kept[name]++
						}
					}
				}
				if required && !present {
					r.missing[name]++
				}
				// A record listed at its new value never goes back to its old one, nor away while o
				// must list it. (A record of the old copy the read did not scan is swept when the
				// read completes and comes back with its live update.)
				if previous != nil && inAfter && charAt(previous.decoded[name], index) == av && c != av && (present || required) {
					r.back[name]++
				}
			}
		}
		if problem := r.problem(o); problem != "" {
			wrong.add(o, problem)
		}
		previous = &obs[i]
		if o.Start >= origin && o.End < stopped && r.fresh.any() && r.lacking.any() {
			claimed.add(o, fmt.Sprintf("records only the new bucket has (%s) while it lacked %s the bucket held", r.fresh, r.lacking))
		}
		if hasFound && o.Start >= found.at && o.End <= readEnd {
			inside++
			if r.fresh.any() && e.partial(o) {
				partial++
			}
		}
		if renewedKept && o.Start > expiresBy && o.End < readEnd {
			keptRenewed++
		}
		if o.Start > whole && complete(o, e.after) {
			exact++
		}
	}
	if claimed.n > 0 {
		v.fail("GET /v1/sessions listed records of the new bucket while the listener still claimed its copy follows the bucket (no line said it had stopped) and lacked records the bucket held, in %d polls from %s to %s; the first: %s",
			claimed.n, since(claimed.first.Start), since(claimed.last.Start), claimed.what)
	}
	if wrong.n > 0 {
		v.fail("GET /v1/sessions answered wrong rows in %d polls from %s to %s; the first: %s", wrong.n, since(wrong.first.Start), since(wrong.last.Start), wrong.what)
	}
	if !hasFound || !hasCompleted {
		return v, nil
	}
	if inside == 0 {
		invalid("no poll fell between the watcher's finding at %s and the read's completion at %s", since(found.at), since(readEnd))
	}
	if renewedKept && keptRenewed == 0 {
		invalid("no poll fell between %s's old record's expiry at %s and the read's completion at %s", *renewed, since(expiresBy), since(readEnd))
	}
	if exact == 0 {
		invalid("no poll came once the read and the fill were both done (%s)", since(whole))
	}
	v.notes = append(v.notes,
		fmt.Sprintf("the old stream ended at sequence %d, the new one at %d", fill.OldLastSeq, fill.NewLastSeq),
		fmt.Sprintf("the watcher's probe found the recreate at %s and its read of the new bucket ran from %s to %s (%d records), before the self-health tick at %s, with no self-health failure or rebuild",
			since(found.at), since(install.at), since(readEnd), completed.entries, since(tick)),
		fmt.Sprintf("%d of %d polls fell between the finding and the read's completion, %d of them with the new bucket partly delivered; every poll listed every record of the old copy until the read completed, and none listed the new bucket's records while the copy claimed to follow",
			inside, len(obs), partial),
		fmt.Sprintf("%s's old record would have expired at %s, after the finding and before the read completed, and the %d polls from then to the read's completion kept it", *renewed, since(expiresBy), keptRenewed),
		fmt.Sprintf("%d polls once the read and the fill were done listed exactly the new bucket", exact))
	return v, nil
}

// liveRows is what one 200 got wrong under judgeRecreateLive's rules, and what it shows of the new
// bucket: fresh counts the records listed at a value only the new bucket has, lacking the records
// the new bucket held by the poll that it does not list at their new value.
type liveRows struct {
	missing, stale, kept, wrong, back, fresh, lacking counts
}

func newLiveRows() liveRows {
	return liveRows{counts{}, counts{}, counts{}, counts{}, counts{}, counts{}, counts{}}
}

func (r liveRows) problem(o observation) string {
	var parts []string
	if r.missing.any() {
		parts = append(parts, "missing "+r.missing.String())
	}
	if r.kept.any() {
		parts = append(parts, "still listing "+r.kept.String())
	}
	if r.stale.any() {
		parts = append(parts, "old values of "+r.stale.String())
	}
	if r.wrong.any() {
		parts = append(parts, "values no seeded record has for "+r.wrong.String())
	}
	if r.back.any() {
		parts = append(parts, "no longer listing at their new value records an earlier poll had: "+r.back.String())
	}
	if o.Unknowns > 0 {
		parts = append(parts, fmt.Sprintf("%d rows the rig never seeded (%s)", o.Unknowns, strings.Join(o.Unknown, " ")))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("a 200 of %d rows %s", o.Rows, strings.Join(parts, "; "))
}

// pollRun gathers the polls one rule found wrong, so a verdict names their span and the first.
type pollRun struct {
	n           int
	first, last observation
	what        string
}

func (p *pollRun) add(o observation, what string) {
	if p.n == 0 {
		p.first, p.what = o, what
	}
	p.last = o
	p.n++
}

// charAt is a poll's character for the record at index, '.' past the end.
func charAt(chars []byte, index int) byte {
	if index < len(chars) {
		return chars[index]
	}
	return '.'
}

// metricsIn finds the metrics scrapes of obs inside [from, to).
func metricsIn(obs []observation, from, to int64) []observation {
	var out []observation
	for _, o := range obs {
		if o.Start >= from && o.End < to {
			out = append(out, o)
		}
	}
	return out
}

func gauge(o observation, list string) *int64 {
	if list == "sessions" {
		return o.Sessions
	}
	return o.Interests
}

func gaugeName(list string) string { return "envoy_active_" + list }

// judgeBoot judges a listener's start on a full bucket (proof P3a): each list answers the
// refilling 503 or a complete 200, never a 200 short of the bucket, and its gauge has a sample only
// when, and equal to what, the list answers.
func judgeBoot(args []string) (verdict, error) {
	flags := flag.NewFlagSet("judge boot", flag.ContinueOnError)
	sessionsPath := flags.String("sessions-obs", "", "GET /v1/sessions polls")
	interestsPath := flags.String("interests-obs", "", "GET /v1/interests/ polls")
	metricsPath := flags.String("metrics-obs", "", "/metrics polls")
	spec := flags.String("sets", "", "sets both buckets hold")
	var v verdict
	if err := flags.Parse(args); err != nil {
		return v, err
	}
	sets, err := newExpectation(*spec, *spec)
	if err != nil {
		return v, err
	}
	metrics, err := readObservations(*metricsPath)
	if err != nil {
		return v, err
	}
	total := int64(sets.total(sets.after))
	metricsAnswered := false
	for _, m := range metrics {
		switch {
		case m.Status == 0 && !metricsAnswered:
			// The listener has not bound its port yet.
		case m.Status != http.StatusOK:
			v.fail("/metrics: a scrape answered %s", answer(m))
		default:
			metricsAnswered = true
		}
	}
	for _, list := range []string{"sessions", "interests"} {
		path := map[string]string{"sessions": *sessionsPath, "interests": *interestsPath}[list]
		obs, err := readObservations(path)
		if err != nil {
			return v, err
		}
		if len(obs) == 0 {
			v.invalid = "no " + list + " poll"
			continue
		}
		origin := obs[0].Start
		answered := false
		refused, whole := 0, 0
		var lastRefused, firstWhole int64 = 0, math.MaxInt64
		for _, o := range obs {
			switch {
			case o.Status == 0 && !answered:
				// The listener has not bound its port yet.
			case refilling(o):
				answered = true
				refused++
				lastRefused = max(lastRefused, o.Start)
			case complete(o, sets.after):
				answered = true
				whole++
				firstWhole = min(firstWhole, o.End)
			default:
				answered = true
				problem := answer(o)
				if o.Status == http.StatusOK {
					problem = sets.rowProblem(o, afterCompletion)
				}
				v.fail("GET %s: %s answered neither the refilling 503 nor the whole bucket: %s", listPaths[list], at(o, origin), problem)
			}
		}
		unsampled := 0
		for _, m := range metrics {
			value := gauge(m, list)
			switch {
			case m.Status != http.StatusOK:
			case value == nil && m.End < lastRefused:
				unsampled++
			case value != nil && *value != total:
				v.fail("%s: %s had the sample %d, and the bucket holds %d", gaugeName(list), at(m, origin), *value, total)
			case value != nil && m.End < lastRefused:
				v.fail("%s: %s had a sample while GET %s still answered 503", gaugeName(list), at(m, origin), listPaths[list])
			case value == nil && m.Start > firstWhole:
				v.fail("%s: %s had no sample once GET %s answered the whole bucket", gaugeName(list), at(m, origin), listPaths[list])
			}
		}
		if refused == 0 || whole == 0 {
			v.invalid = fmt.Sprintf("GET %s answered the refilling 503 %d times and the whole bucket %d times: the polls missed the boot", listPaths[list], refused, whole)
		}
		v.notes = append(v.notes, fmt.Sprintf("GET %s: %d refilling 503s then %d complete 200s of %d records; %s had no sample in the %d scrapes before its list's last 503 and %d after its first complete 200",
			listPaths[list], refused, whole, total, gaugeName(list), unsampled, sampledAfter(metrics, list, firstWhole, total)))
	}
	return v, nil
}

// sampledAfter counts the scrapes begun after from whose gauge for list has the sample total.
func sampledAfter(metrics []observation, list string, from, total int64) int {
	n := 0
	for _, m := range metrics {
		if value := gauge(m, list); m.Start > from && value != nil && *value == total {
			n++
		}
	}
	return n
}

// judgeOutage judges a NATS outage past the stale bound and the recovery after it (proof P3b):
// each list serves its copy within the bound, answers the refilling 503 past it with no gauge
// sample, and once NATS is back answers a complete 200 within the recovery limit, its gauge equal
// to its length.
func judgeOutage(args []string) (verdict, error) {
	flags := flag.NewFlagSet("judge outage", flag.ContinueOnError)
	sessionsPath := flags.String("sessions-obs", "", "GET /v1/sessions polls")
	interestsPath := flags.String("interests-obs", "", "GET /v1/interests/ polls")
	metricsPath := flags.String("metrics-obs", "", "/metrics polls")
	spec := flags.String("sets", "", "sets both buckets hold")
	down := flags.Int64("down-ns", 0, "when NATS was stopped")
	restarted := flags.Int64("restart-ns", 0, "when NATS was started again")
	seeded := flags.Int64("seeded-ns", 0, "when the records were written")
	bound := flags.Duration("bound", 2*time.Minute, "the stale bound")
	margin := flags.Duration("margin", 5*time.Second, "slack around the bound")
	within := flags.Duration("within", 30*time.Second, "how soon after NATS is back the list must be whole")
	ttl := flags.Duration("ttl", 5*time.Minute, "the session bucket's record lifetime")
	var v verdict
	if err := flags.Parse(args); err != nil {
		return v, err
	}
	if *restarted-*seeded > int64(*ttl-30*time.Second) {
		v.invalid = fmt.Sprintf("NATS came back %s after the records were written, too close to their %s lifetime for the list to be whole", time.Duration(*restarted-*seeded).Round(time.Second), *ttl)
		return v, nil
	}
	sets, err := newExpectation(*spec, *spec)
	if err != nil {
		return v, err
	}
	metrics, err := readObservations(*metricsPath)
	if err != nil {
		return v, err
	}
	total := int64(sets.total(sets.after))
	refuseFrom := *down + int64(*bound+*margin)
	serveUntil := *down + int64(*bound-*margin)
	for _, list := range []string{"sessions", "interests"} {
		path := map[string]string{"sessions": *sessionsPath, "interests": *interestsPath}[list]
		obs, err := readObservations(path)
		if err != nil {
			return v, err
		}
		served, refused, firstRefused := 0, 0, int64(0)
		var recovered int64
		for _, o := range obs {
			switch {
			case o.End < serveUntil:
				if !complete(o, sets.after) {
					v.fail("GET %s: %s, %s after NATS stopped and within the bound, answered %s instead of its copy", listPaths[list], at(o, *down), time.Duration(o.Start-*down).Round(time.Second), describe(o, sets))
				} else {
					served++
				}
			case o.Start > refuseFrom && o.End < *restarted:
				if !refilling(o) {
					v.fail("GET %s: %s, %s after NATS stopped and past the %s bound, answered %s instead of the refilling 503", listPaths[list], at(o, *down), time.Duration(o.Start-*down).Round(time.Second), *bound, describe(o, sets))
				} else {
					refused++
				}
			case o.Start > *restarted:
				switch {
				case complete(o, sets.after):
					if recovered == 0 {
						recovered = o.End
					}
				case refilling(o) && recovered == 0:
				default:
					v.fail("GET %s: %s, after NATS came back, answered %s", listPaths[list], at(o, *down), describe(o, sets))
				}
			}
			if refilling(o) && firstRefused == 0 && o.Start > *down {
				firstRefused = o.Start
			}
		}
		if refused == 0 || served == 0 {
			v.invalid = fmt.Sprintf("GET %s: %d polls within the bound and %d past it: the polls missed the outage", listPaths[list], served, refused)
		}
		switch {
		case recovered == 0:
			v.fail("GET %s never answered the whole bucket after NATS came back", listPaths[list])
		case recovered-*restarted > int64(*within):
			v.fail("GET %s answered the whole bucket %s after NATS came back, later than %s", listPaths[list], time.Duration(recovered-*restarted).Round(time.Millisecond), *within)
		}
		for _, m := range metrics {
			value := gauge(m, list)
			switch {
			case m.Start > refuseFrom && m.End < *restarted:
				if value != nil {
					v.fail("%s: %s, past the bound with NATS down, had the sample %d", gaugeName(list), at(m, *down), *value)
				}
			case recovered != 0 && m.Start > recovered:
				if m.Status != http.StatusOK || value == nil || *value != total {
					v.fail("%s: %s, after GET %s answered %d records, had %s", gaugeName(list), at(m, *down), listPaths[list], total, sampleText(m, value))
				}
			}
		}
		v.notes = append(v.notes, fmt.Sprintf("GET %s: its copy for %d polls within the bound, the refilling 503 from %s after NATS stopped (%d polls past the bound), whole again %s after NATS came back",
			listPaths[list], served, time.Duration(firstRefused-*down).Round(time.Second), refused, time.Duration(recovered-*restarted).Round(time.Millisecond)))
	}
	unsampled := 0
	for _, m := range metricsIn(metrics, refuseFrom, *restarted) {
		if m.Status == http.StatusOK && m.Sessions == nil && m.Interests == nil {
			unsampled++
		}
	}
	if unsampled == 0 && v.invalid == "" {
		v.invalid = "no /metrics scrape fell past the bound while NATS was down"
	}
	v.notes = append(v.notes, fmt.Sprintf("%d /metrics scrapes past the bound had neither gauge's sample; after recovery both equal %d", unsampled, total))
	return v, nil
}

func describe(o observation, sets expectation) string {
	if o.Status == http.StatusOK && o.Transport == "" {
		if problem := sets.rowProblem(o, afterCompletion); problem != "" {
			return problem
		}
		return fmt.Sprintf("a 200 of all %d records", o.Rows)
	}
	return answer(o)
}

func sampleText(m observation, value *int64) string {
	if m.Status != http.StatusOK {
		return answer(m)
	}
	if value == nil {
		return "no sample"
	}
	return fmt.Sprintf("the sample %d", *value)
}
