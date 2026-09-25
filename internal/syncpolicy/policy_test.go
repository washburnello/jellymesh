package syncpolicy

import (
	"errors"
	"testing"
	"time"
)

func TestDefaultPolicy(t *testing.T) {
	policy := DefaultPolicy()
	if err := policy.Validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	if policy.HealthInterval != 5*time.Minute || policy.CatalogInterval != time.Hour || policy.OfflineCatalogRetention != 15*24*time.Hour {
		t.Fatalf("unexpected default intervals: %#v", policy)
	}
}

func TestBackoffIsBounded(t *testing.T) {
	policy := DefaultPolicy()
	if backoff := policy.Backoff(0); backoff != time.Second {
		t.Fatalf("unexpected initial backoff: %s", backoff)
	}
	if backoff := policy.Backoff(20); backoff != policy.MaximumBackoff {
		t.Fatalf("unexpected capped backoff: %s", backoff)
	}
}

func TestPolicyValidation(t *testing.T) {
	if err := (Policy{}).Validate(); !errors.Is(err, ErrInvalidHealthInterval) {
		t.Fatalf("unexpected zero-policy error: %v", err)
	}
}

func TestBackoffWithJitterStaysInBounds(t *testing.T) {
	policy := DefaultPolicy()
	for attempt := 0; attempt < 12; attempt++ {
		for _, r := range []float64{0, 0.5, 0.999} {
			got := policy.BackoffWithJitter(attempt, func() float64 { return r })
			if got < time.Second {
				t.Fatalf("attempt %d r %v: below floor: %v", attempt, r, got)
			}
			if got > policy.MaximumBackoff {
				t.Fatalf("attempt %d r %v: above ceiling: %v", attempt, r, got)
			}
		}
	}
}

func TestBackoffWithJitterActuallyVaries(t *testing.T) {
	policy := DefaultPolicy()
	low := policy.BackoffWithJitter(5, func() float64 { return 0 })
	high := policy.BackoffWithJitter(5, func() float64 { return 0.999 })
	if low == high {
		t.Fatal("jitter produced identical delays; retries would stay synchronized")
	}
}

func TestBackoffWithNilRandomIsDeterministic(t *testing.T) {
	policy := DefaultPolicy()
	if policy.BackoffWithJitter(4, nil) != policy.Backoff(4) {
		t.Fatal("nil random source should fall back to the un-jittered delay")
	}
}
