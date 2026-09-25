package identity

import (
	"errors"
	"testing"
)

func TestStrongIdentityMatch(t *testing.T) {
	first, err := NewWorkIdentity("movie", "tmdb", "12345")
	if err != nil {
		t.Fatalf("create first identity: %v", err)
	}
	second, err := NewWorkIdentity("MOVIE", "TMDB", "12345")
	if err != nil {
		t.Fatalf("create second identity: %v", err)
	}
	if !first.StronglyMatches(second) {
		t.Fatal("same strong identity should match")
	}
}

func TestEmptyProviderIDIsRejected(t *testing.T) {
	_, err := NewWorkIdentity("movie", "", "The Movie")
	if !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("unexpected invalid identity error: %v", err)
	}
}

func TestDifferentStrongIdentitiesDoNotMatch(t *testing.T) {
	first, err := NewWorkIdentity("movie", "tmdb", "12345")
	if err != nil {
		t.Fatalf("create first identity: %v", err)
	}
	second, err := NewWorkIdentity("movie", "tmdb", "67890")
	if err != nil {
		t.Fatalf("create second identity: %v", err)
	}
	if first.StronglyMatches(second) {
		t.Fatal("different provider IDs should not match")
	}
}

func TestInvalidIdentity(t *testing.T) {
	_, err := NewWorkIdentity("", "tmdb", "12345")
	if !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("unexpected invalid identity error: %v", err)
	}
}
