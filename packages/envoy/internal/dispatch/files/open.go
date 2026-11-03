package files

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// GCSScheme marks a setting that names a Google Cloud Storage bucket: gs://<bucket>.
const GCSScheme = "gs://"

// Open is the store a setting names (cmd/dispatch's DISPATCH_FILE_STORE_BUCKET): gs://<bucket>
// is a Google Cloud Storage bucket (NewGCS), and any other value is an S3 bucket's name (NewS3).
// The server and the backfill both open their store here, so the two can never read one value
// as two stores. A failure answers a nil Store, never a typed nil.
func Open(ctx context.Context, setting string) (Store, error) {
	if setting == "" {
		return nil, errors.New("the file store setting names no bucket")
	}
	if bucket, ok := strings.CutPrefix(setting, GCSScheme); ok {
		store, err := NewGCS(ctx, bucket)
		if err != nil {
			return nil, err
		}
		return store, nil
	}
	store, err := NewS3(ctx, setting)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// validGCSBucket refuses a name Cloud Storage would never give a bucket, so a typo, or a path
// after the bucket (gs://bucket/prefix, which the store does not take), fails the boot rather
// than the first upload: 3 to 63 characters of lowercase letters, digits, '-', '_' and '.', or up
// to 222 when it holds dots, each dot-separated part 1 to 63 long; starting and ending with a
// letter or digit; not an IPv4 address; not starting with "goog" or holding "google".
func validGCSBucket(bucket string) error {
	refuse := func(why string) error {
		return fmt.Errorf("file store %q: %q is not a Cloud Storage bucket name: %s", GCSScheme+bucket, bucket, why)
	}
	if bucket == "" {
		return refuse("it is empty")
	}
	limit := 63
	if strings.Contains(bucket, ".") {
		limit = 222
	}
	if len(bucket) < 3 || len(bucket) > limit {
		return refuse(fmt.Sprintf("it must be 3 to %d characters long", limit))
	}
	for _, r := range bucket {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return refuse(fmt.Sprintf("it holds %q; a bucket name holds only lowercase letters, digits, '-', '_' and '.'", r))
		}
	}
	alnum := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' }
	if !alnum(bucket[0]) || !alnum(bucket[len(bucket)-1]) {
		return refuse("it must start and end with a letter or digit")
	}
	for part := range strings.SplitSeq(bucket, ".") {
		if part == "" || len(part) > 63 {
			return refuse("each part between dots must be 1 to 63 characters long")
		}
	}
	if addr, err := netip.ParseAddr(bucket); err == nil && addr.Is4() {
		return refuse("it is an IP address")
	}
	if strings.HasPrefix(bucket, "goog") || strings.Contains(bucket, "google") {
		return refuse(`it starts with "goog" or holds "google"`)
	}
	return nil
}
