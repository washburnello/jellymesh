package relay

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"jellymesh/internal/store"
)

// Jellyfin probes a remote item on every PlaybackInfo, which clients send
// when an item's page opens, and each probe reads about a megabyte from the
// start of the file (phase-0-results.md, A1-a). Without a cache, every page
// view crosses the source's uplink. The relay therefore keeps the first bytes
// of each item it has served, per revision, and answers requests that start
// within them locally (C-PB-5).

// DefaultHeadSize is how much of an item's start is kept.
const DefaultHeadSize = 4 << 20

// Head is the cached start of one revision of one item.
type Head struct {
	Data        []byte
	Total       int64
	ContentType string
}

// HeadCache keeps heads on disk under dir, at most perItem bytes each and
// capacity bytes in all, evicting the least recently used.
type HeadCache struct {
	dir      string
	perItem  int
	capacity int64
	mutex    sync.Mutex
}

// NewHeadCache returns a cache in dir.
func NewHeadCache(dir string, perItem int, capacity int64) (*HeadCache, error) {
	if perItem <= 0 || capacity < int64(perItem) {
		return nil, errors.New("the head cache needs a positive size and room for at least one head")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &HeadCache{dir: dir, perItem: perItem, capacity: capacity}, nil
}

func itemDirectory(record store.Materialized) string {
	sum := sha256.Sum256([]byte(record.SourceNodeID + "/" + record.ItemID))
	return hex.EncodeToString(sum[:16])
}

func (cache *HeadCache) path(record store.Materialized) string {
	return filepath.Join(cache.dir, itemDirectory(record), strconv.FormatUint(record.Revision, 10))
}

// Get returns the head for a record's revision, if one is kept.
func (cache *HeadCache) Get(record store.Materialized) (Head, bool) {
	path := cache.path(record)
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 10 {
		return Head{}, false
	}
	total := int64(binary.BigEndian.Uint64(data[:8]))
	typeLength := int(binary.BigEndian.Uint16(data[8:10]))
	if len(data) < 10+typeLength || total <= 0 {
		return Head{}, false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now) // recency for eviction
	return Head{Total: total, ContentType: string(data[10 : 10+typeLength]), Data: data[10+typeLength:]}, true
}

// Put keeps a head, replacing a shorter one, and evicts old heads beyond
// capacity.
func (cache *HeadCache) Put(record store.Materialized, head Head) error {
	if len(head.Data) > cache.perItem {
		head.Data = head.Data[:cache.perItem]
	}
	if len(head.Data) == 0 || head.Total <= 0 || int64(len(head.Data)) > head.Total {
		return nil
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if existing, ok := cache.Get(record); ok && existing.Total == head.Total && len(existing.Data) >= len(head.Data) {
		return nil
	}
	path := cache.path(record)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded := make([]byte, 10, 10+len(head.ContentType)+len(head.Data))
	binary.BigEndian.PutUint64(encoded[:8], uint64(head.Total))
	binary.BigEndian.PutUint16(encoded[8:10], uint16(len(head.ContentType)))
	encoded = append(encoded, head.ContentType...)
	encoded = append(encoded, head.Data...)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return cache.evict()
}

// Remove drops every kept revision of a record's item.
func (cache *HeadCache) Remove(record store.Materialized) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	os.RemoveAll(filepath.Join(cache.dir, itemDirectory(record)))
}

// Size reports the bytes kept, for tests and status.
func (cache *HeadCache) Size() int64 {
	var total int64
	filepath.WalkDir(cache.dir, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if info, err := entry.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func (cache *HeadCache) evict() error {
	type file struct {
		path     string
		size     int64
		modified time.Time
	}
	var files []file
	var total int64
	err := filepath.WalkDir(cache.dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files = append(files, file{path, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan head cache: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
	for _, oldest := range files {
		if total <= cache.capacity {
			break
		}
		if os.Remove(oldest.path) == nil {
			total -= oldest.size
			os.Remove(filepath.Dir(oldest.path)) // only if now empty
		}
	}
	return nil
}
