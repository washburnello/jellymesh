package settings

import (
	"errors"
	"testing"

	"jellymesh/internal/limits"
)

func TestDefaultSettings(t *testing.T) {
	settings := Default()
	if err := settings.Validate(); err != nil {
		t.Fatalf("default settings invalid: %v", err)
	}
	if settings.RemoteTranscodeLimit != 1 || !settings.ShowSyncHealth {
		t.Fatalf("unexpected default settings: %+v", settings)
	}
}

func TestGroupDefaultAppliesWithoutLocalOverride(t *testing.T) {
	settings := Default()
	if settings.EffectiveRemoteTranscodeLimit(2) != 2 {
		t.Fatal("group default should apply without a local override")
	}
}

func TestLocalRemoteTranscodeOverrideWins(t *testing.T) {
	settings := Default()
	settings.RemoteTranscodeLimit = 3
	settings.RemoteTranscodeOverrideSet = true
	if settings.EffectiveRemoteTranscodeLimit(2) != 3 {
		t.Fatal("local override should win over group default")
	}
}

func TestSourceLibraryOptOut(t *testing.T) {
	settings := Default()
	source := SourceLibrary{ServerID: "walnut", LibraryID: "shared-movies"}
	settings.SourceOptOuts[source] = true
	if !settings.IsOptedOut(source) {
		t.Fatal("source library opt-out should be visible")
	}
}

func TestInvalidSettings(t *testing.T) {
	settings := Default()
	settings.RemoteTranscodeLimit = 0
	if err := settings.Validate(); !errors.Is(err, limits.ErrInvalidConcurrentTranscodes) {
		t.Fatalf("unexpected transcode error: %v", err)
	}

	settings = Default()
	settings.PublishedLibraryIDs = []string{""}
	if err := settings.Validate(); !errors.Is(err, ErrInvalidPublishedLibrary) {
		t.Fatalf("unexpected published library error: %v", err)
	}
}
