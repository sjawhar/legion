package rules

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Loader interface {
	Load(ctx context.Context) ([]byte, error)
}

type FileLoader struct{ Path string }

func (l FileLoader) Load(context.Context) ([]byte, error) {
	data, err := os.ReadFile(l.Path)
	if err != nil {
		return nil, fmt.Errorf("rules file %s: %w", l.Path, err)
	}
	return data, nil
}

type s3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type S3Loader struct {
	Client s3API
	Bucket string
	Key    string
}

// ParseS3URI splits s3://<bucket>/<key>; anything else is refused naming the value.
func ParseS3URI(uri string) (string, string, error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	bucket, key, found := strings.Cut(rest, "/")
	if !ok || !found || bucket == "" || key == "" {
		return "", "", fmt.Errorf("rules S3 URI must be s3://<bucket>/<key>, got %q", uri)
	}
	return bucket, key, nil
}

func (l S3Loader) Load(ctx context.Context) ([]byte, error) {
	out, err := l.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(l.Bucket), Key: aws.String(l.Key)})
	if err != nil {
		return nil, fmt.Errorf("rules object s3://%s/%s: %w", l.Bucket, l.Key, err)
	}
	defer out.Body.Close()
	const maxSize = 4 << 20
	data, err := io.ReadAll(io.LimitReader(out.Body, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("rules object s3://%s/%s: %w", l.Bucket, l.Key, err)
	}
	if len(data) > maxSize {
		return nil, fmt.Errorf("rules object s3://%s/%s: exceeds %d byte limit", l.Bucket, l.Key, maxSize)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("rules object s3://%s/%s: empty", l.Bucket, l.Key)
	}
	return data, nil
}

// Current is the live rule set. The first load must succeed; a later failed load or invalid file
// keeps the previous set and calls alarm, so a bad edit never turns into deny-everything or
// allow-everything.
type Current struct {
	set atomic.Pointer[Set]
}

// NewCurrent loads loader once (a failure here is fatal) and then reloads on a ticker.
func NewCurrent(ctx context.Context, loader Loader, reload time.Duration, alarm func(error)) (*Current, error) {
	c := &Current{}
	set, err := load(ctx, loader)
	if err != nil {
		return nil, err
	}
	c.set.Store(set)
	go func() {
		ticker := time.NewTicker(reload)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				next, err := load(ctx, loader)
				if err != nil {
					alarm(err)
					continue
				}
				c.set.Store(next)
			}
		}
	}()
	return c, nil
}

func load(ctx context.Context, loader Loader) (*Set, error) {
	data, err := loader.Load(ctx)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func (c *Current) Get() *Set { return c.set.Load() }
