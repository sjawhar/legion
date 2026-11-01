package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

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
