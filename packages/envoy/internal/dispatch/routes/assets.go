package routes

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/bytelru"
)

const (
	// MaxRetainedAssetSize bounds one dashboard asset: what one request may read from the store,
	// and so the memory one store read holds while it is answered. internal/dispatch/dashboard
	// refuses to publish a build holding a larger file, so the store holds none.
	MaxRetainedAssetSize int64 = 8 << 20
	// retainedAssetFetchTimeout bounds the whole read of an asset from the store. The browser's
	// download of the bytes read is not bounded by it.
	retainedAssetFetchTimeout = 3 * time.Second
	// maxRetainedBytesHeld caps the bytes of store reads the handler holds at once. A client that
	// stops reading keeps its object in memory, and the server sets no write timeout.
	maxRetainedBytesHeld int64 = 64 << 20
	// maxCachedAssetBytes caps the bytes the process keeps of the objects the store answered
	// (assetCache).
	maxCachedAssetBytes int64 = 64 << 20
)

// ErrAssetNotFound distinguishes an asset the store does not hold from a store failure.
var ErrAssetNotFound = errors.New("retained asset not found")

// AssetStore holds the hashed assets of every dashboard build a server process has published
// (internal/dispatch/dashboard), this process's own among them. The static handler asks it only
// when this process's build does not hold the file a browser asks for: a page another process
// served, of another build, while a rolling update or a rollback runs two builds at once.
type AssetStore interface {
	// GetAsset returns the whole object stored under key, at most MaxRetainedAssetSize bytes, or
	// ErrAssetNotFound when there is none. Any other error is a store failure.
	GetAsset(ctx context.Context, key string) ([]byte, error)
}

// assetCache keeps the objects the store answered, whole and by key, so a burst of one stale
// tab's chunks, or every tab of another build asking for the same ones, reads each from the store
// once. A key names content-hashed build output, which never changes under its name, so an entry
// is never stale. It holds at most the bytes it was made with, evicting the least recently used
// entry first (bytelru), and collapses concurrent misses of one key into one store read (reads).
type assetCache struct {
	*bytelru.Cache[string, []byte]
	reads singleflight.Group
}

func newAssetCache(limit int64) *assetCache {
	return &assetCache{Cache: bytelru.New[string, []byte](limit)}
}
