package proof

import (
	"context"
	"errors"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Verifier, string, string) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tp, err := Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	v := &Verifier{
		Skew: time.Minute,
		Lookup: func(_ context.Context, id string) (string, bool, error) {
			if id == "enr-1" {
				return tp, true, nil
			}
			return "", false, nil
		},
		Replay: func(_ context.Context, jti string, _ time.Time) (bool, error) {
			if seen[jti] {
				return false, nil
			}
			seen[jti] = true
			return true, nil
		},
	}
	now := time.Now()
	compact, err := Sign(key, "enr-1", "POST", "https://secrets.test/v1/requests", now)
	if err != nil {
		t.Fatal(err)
	}
	return v, compact, tp
}

func TestValidProofPasses(t *testing.T) {
	v, compact, _ := fixture(t)
	id, err := v.Verify(context.Background(), compact, "POST", "https://secrets.test/v1/requests", time.Now())
	if err != nil || id != "enr-1" {
		t.Fatalf("expected enr-1, got %q %v", id, err)
	}
}

func TestReplayedJtiFails(t *testing.T) {
	v, compact, _ := fixture(t)
	ctx := context.Background()
	if _, err := v.Verify(ctx, compact, "POST", "https://secrets.test/v1/requests", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, compact, "POST", "https://secrets.test/v1/requests", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("replay must fail with ErrInvalid, got %v", err)
	}
}

func TestWrongURLFails(t *testing.T) {
	v, compact, _ := fixture(t)
	if _, err := v.Verify(context.Background(), compact, "POST", "https://secrets.test/v1/grants/x/values", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("htu mismatch must fail, got %v", err)
	}
}

func TestOtherKeyForSameEnrollmentFails(t *testing.T) {
	v, _, _ := fixture(t)
	other, _ := NewKey()
	compact, _ := Sign(other, "enr-1", "POST", "https://secrets.test/v1/requests", time.Now())
	if _, err := v.Verify(context.Background(), compact, "POST", "https://secrets.test/v1/requests", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("thumbprint mismatch must fail, got %v", err)
	}
}

func TestSkewedProofFails(t *testing.T) {
	v, compact, _ := fixture(t)
	if _, err := v.Verify(context.Background(), compact, "POST", "https://secrets.test/v1/requests", time.Now().Add(5*time.Minute)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("iat outside skew must fail, got %v", err)
	}
}
