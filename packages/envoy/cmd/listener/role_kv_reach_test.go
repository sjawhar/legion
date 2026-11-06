package main

import (
	"reflect"
	"testing"
	"unsafe"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/store"
)

// registryRoleKV and setRegistryRoleKV reach into store.Registry's unexported roleKV field so a
// test can stall or wrap the role bucket the same way internal/store/kv_test.go does from inside
// package store.
func registryRoleKV(t *testing.T, registry *store.Registry) natsgo.KeyValue {
	t.Helper()
	field := reflect.ValueOf(registry).Elem().FieldByName("roleKV")
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(bus.KeyValue).Legacy()
}

func setRegistryRoleKV(t *testing.T, registry *store.Registry, kv natsgo.KeyValue) {
	t.Helper()
	field := reflect.ValueOf(registry).Elem().FieldByName("roleKV")
	current := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	// The replacement keeps the bucket's jetstream handle, which every scan of it goes through.
	base := current.Interface().(bus.KeyValue)
	current.Set(reflect.ValueOf(bus.NewKeyValue(kv, base.WatchHandle(), base.Streams())))
}
