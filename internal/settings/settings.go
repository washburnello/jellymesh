package settings

import (
	"errors"

	"jellymesh/internal/limits"
)

var (
	ErrInvalidPublishedLibrary = errors.New("published library ID cannot be empty")
	ErrInvalidSourceOptOut      = errors.New("source opt-out requires server and library IDs")
)

type SourceLibrary struct {
	ServerID  string
	LibraryID string
}

type Settings struct {
	PublishedLibraryIDs          []string
	SourceOptOuts                map[SourceLibrary]bool
	RemoteTranscodeLimit         int
	RemoteTranscodeOverrideSet   bool
	ShowSyncHealth               bool
}

func Default() Settings {
	return Settings{
		PublishedLibraryIDs:  []string{},
		SourceOptOuts:        map[SourceLibrary]bool{},
		RemoteTranscodeLimit: limits.DefaultConcurrentRemoteTranscodes,
		ShowSyncHealth:       true,
	}
}

func (settings Settings) Validate() error {
	if settings.RemoteTranscodeLimit <= 0 {
		return limits.ErrInvalidConcurrentTranscodes
	}
	for _, libraryID := range settings.PublishedLibraryIDs {
		if libraryID == "" {
			return ErrInvalidPublishedLibrary
		}
	}
	for sourceLibrary := range settings.SourceOptOuts {
		if sourceLibrary.ServerID == "" || sourceLibrary.LibraryID == "" {
			return ErrInvalidSourceOptOut
		}
	}
	return nil
}

func (settings Settings) EffectiveRemoteTranscodeLimit(groupDefault int) int {
	if settings.RemoteTranscodeOverrideSet || groupDefault <= 0 {
		return settings.RemoteTranscodeLimit
	}
	return groupDefault
}

func (settings Settings) IsOptedOut(sourceLibrary SourceLibrary) bool {
	return settings.SourceOptOuts[sourceLibrary]
}
