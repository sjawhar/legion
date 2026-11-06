package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	"syscall"
	"time"
)

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
