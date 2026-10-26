package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

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
