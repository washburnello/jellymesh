package limits

import (
	"errors"
	"testing"
)

func TestDefaultPolicyAllowsOneRemoteTranscode(t *testing.T) {
	policy := DefaultPolicy()
	if err := policy.Validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	if !policy.CanStartRemoteTranscode(0) {
		t.Fatal("first remote transcode should be allowed")
	}
	if policy.CanStartRemoteTranscode(1) {
		t.Fatal("second remote transcode should be limited by default")
	}
}

func TestPolicyCanBeRaised(t *testing.T) {
	policy := Policy{ConcurrentRemoteTranscodes: 2}
	if !policy.CanStartRemoteTranscode(1) {
		t.Fatal("second remote transcode should be allowed with an override")
	}
}

func TestInvalidPolicy(t *testing.T) {
	if err := (Policy{}).Validate(); !errors.Is(err, ErrInvalidConcurrentTranscodes) {
		t.Fatalf("unexpected invalid policy error: %v", err)
	}
}
