package store

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/routing"
)

const Bucket = "envoy_interests"

type Registry struct {
	kv nats.KeyValue
}

func Open(conn *nats.Conn) (Registry, error) {
	js, err := conn.JetStream()
	if err != nil {
		return Registry{}, err
	}
	kv, err := js.KeyValue(Bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: Bucket})
	}
	if err != nil {
		return Registry{}, err
	}
	return Registry{kv: kv}, nil
}

func (r Registry) Upsert(item Interest, topics []string) (Interest, error) {
	cur, err := r.Get(item.SessionID)
	if err == nil {
		item = cur
	}
	item.MachineID = first(item.MachineID, curValue(cur.MachineID))
	item.Dir = first(item.Dir, curValue(cur.Dir))
	item.UpdatedAt = time.Now().UnixMilli()
	item = Merge(item, topics)
	sort.Strings(item.Topics)
	buf, err := json.Marshal(item)
	if err != nil {
		return Interest{}, err
	}
	_, err = r.kv.Put(item.SessionID, buf)
	return item, err
}

func (r Registry) Remove(sessionID string, topics []string) error {
	item, err := r.Get(sessionID)
	if err != nil {
		return err
	}
	item = Remove(item, topics)
	if len(item.Topics) == 0 {
		return r.kv.Delete(sessionID)
	}
	item.UpdatedAt = time.Now().UnixMilli()
	buf, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = r.kv.Put(sessionID, buf)
	return err
}

func (r Registry) Get(sessionID string) (Interest, error) {
	entry, err := r.kv.Get(sessionID)
	if err != nil {
		return Interest{}, err
	}
	var item Interest
	err = json.Unmarshal(entry.Value(), &item)
	return item, err
}

func (r Registry) Match(machineID string, topic string) ([]Interest, error) {
	keys, err := r.kv.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Interest{}
	for _, key := range keys {
		item, err := r.Get(key)
		if err != nil {
			continue
		}
		if item.MachineID != machineID {
			continue
		}
		for _, pattern := range item.Topics {
			if routing.Match(pattern, topic) {
				out = append(out, item)
				break
			}
		}
	}
	return out, nil
}

func first(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func curValue(value string) string {
	return value
}
