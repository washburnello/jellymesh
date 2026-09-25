package history

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrUserIDRequired        = errors.New("user ID is required")
	ErrWorkIDRequired        = errors.New("logical work ID is required")
	ErrInvalidPosition       = errors.New("resume position must not be negative")
	ErrInvalidPlayCount      = errors.New("play count must not be negative")
	ErrHistoryIdentityMismatch = errors.New("history entries refer to different users or works")
)

type Entry struct {
	UserID            string
	WorkID            string
	MediaType         string
	ProviderIdentity  string
	Played            bool
	PlayCount         int
	ResumePositionTicks int64
	LastPlayedAt      time.Time
	UpdatedAt         time.Time
}

type Ledger struct {
	entries map[string]map[string]Entry
}

func NewLedger() *Ledger {
	return &Ledger{entries: make(map[string]map[string]Entry)}
}

func (ledger *Ledger) Save(entry Entry) error {
	if ledger == nil {
		return errors.New("history ledger is nil")
	}
	entry.UserID = strings.TrimSpace(entry.UserID)
	entry.WorkID = strings.TrimSpace(entry.WorkID)
	entry.MediaType = strings.TrimSpace(entry.MediaType)
	entry.ProviderIdentity = strings.TrimSpace(entry.ProviderIdentity)
	if entry.UserID == "" {
		return ErrUserIDRequired
	}
	if entry.WorkID == "" {
		return ErrWorkIDRequired
	}
	if entry.ResumePositionTicks < 0 {
		return ErrInvalidPosition
	}
	if entry.PlayCount < 0 {
		return ErrInvalidPlayCount
	}
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = time.Now().UTC()
	}
	if ledger.entries[entry.UserID] == nil {
		ledger.entries[entry.UserID] = make(map[string]Entry)
	}
	ledger.entries[entry.UserID][entry.WorkID] = entry
	return nil
}

func MergePreservingLocal(local Entry, preserved Entry) (Entry, error) {
	local.UserID = strings.TrimSpace(local.UserID)
	local.WorkID = strings.TrimSpace(local.WorkID)
	preserved.UserID = strings.TrimSpace(preserved.UserID)
	preserved.WorkID = strings.TrimSpace(preserved.WorkID)
	if local.UserID != preserved.UserID || local.WorkID != preserved.WorkID {
		return Entry{}, ErrHistoryIdentityMismatch
	}
	if local.UserID == "" {
		return Entry{}, ErrUserIDRequired
	}
	if local.WorkID == "" {
		return Entry{}, ErrWorkIDRequired
	}
	if local.ResumePositionTicks < 0 || preserved.ResumePositionTicks < 0 {
		return Entry{}, ErrInvalidPosition
	}
	if local.PlayCount < 0 || preserved.PlayCount < 0 {
		return Entry{}, ErrInvalidPlayCount
	}
	// The preserved ledger entry may only contribute progress that the local
	// record does not already account for. When the local record is the more
	// recently updated of the two, it is authoritative in full: a user who
	// deliberately marked a work unwatched must not have that undone when the
	// work returns from another source.
	localIsNewer := local.UpdatedAt.After(preserved.UpdatedAt)

	if !local.Played && local.ResumePositionTicks == 0 && !localIsNewer {
		local.ResumePositionTicks = preserved.ResumePositionTicks
	}
	if !localIsNewer {
		local.Played = local.Played || preserved.Played
		if preserved.PlayCount > local.PlayCount {
			local.PlayCount = preserved.PlayCount
		}
	}
	if preserved.LastPlayedAt.After(local.LastPlayedAt) {
		local.LastPlayedAt = preserved.LastPlayedAt
	}
	if preserved.UpdatedAt.After(local.UpdatedAt) {
		local.UpdatedAt = preserved.UpdatedAt
	}
	if strings.TrimSpace(local.MediaType) == "" {
		local.MediaType = preserved.MediaType
	}
	if strings.TrimSpace(local.ProviderIdentity) == "" {
		local.ProviderIdentity = preserved.ProviderIdentity
	}
	return local, nil
}

func (ledger *Ledger) Get(userID string, workID string) (Entry, bool) {
	if ledger == nil {
		return Entry{}, false
	}
	works, ok := ledger.entries[strings.TrimSpace(userID)]
	if !ok {
		return Entry{}, false
	}
	entry, ok := works[strings.TrimSpace(workID)]
	return entry, ok
}

func (ledger *Ledger) WorksForUser(userID string) []Entry {
	if ledger == nil {
		return nil
	}
	works, ok := ledger.entries[strings.TrimSpace(userID)]
	if !ok {
		return nil
	}
	entries := make([]Entry, 0, len(works))
	for _, entry := range works {
		entries = append(entries, entry)
	}
	return entries
}
