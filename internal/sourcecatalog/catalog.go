// Package sourcecatalog is the source side of the catalog protocol
// (design-spec section 9, "Catalog protocol"): what this node publishes, the
// catalog of items it offers, and the checks every request for them passes.
//
// Nothing is exposed that was not explicitly published. A library is
// published by its stable Jellyfin ID with declared root paths, after the
// service user is shown to see it, it is shown not to be protected, and every
// current item is shown to lie under the roots. Every refresh repeats the root
// check and pauses a publication that no longer passes it.
package sourcecatalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"jellymesh/internal/audit"
	"jellymesh/internal/jellyfin"
	"jellymesh/internal/store"
)

var (
	ErrProtected    = errors.New("the library is protected and can never be published")
	ErrNotVisible   = errors.New("the service user cannot see that library, so it cannot be published")
	ErrOutsideRoots = errors.New("items lie outside the declared roots")
	ErrNoRoots      = errors.New("a publication must declare at least one absolute root path")
	ErrNotPublished = errors.New("the item is not in a published library")
)

// DefaultPageSize is how many items a refresh asks Jellyfin for at once.
const DefaultPageSize = 500

// Adapter is the part of the Jellyfin client the catalog uses.
type Adapter interface {
	Libraries(ctx context.Context) ([]jellyfin.Library, error)
	Items(ctx context.Context, libraryID string, startIndex int, limit int) (jellyfin.Page, error)
	LibraryOf(ctx context.Context, itemID string) (string, error)
}

// Metadata is what a destination receives about an item. It carries no path,
// ETag, or other detail of the source's filesystem.
type Metadata struct {
	Name              string            `json:"name"`
	OriginalTitle     string            `json:"original_title,omitempty"`
	Overview          string            `json:"overview,omitempty"`
	OfficialRating    string            `json:"official_rating,omitempty"`
	Year              int               `json:"year,omitempty"`
	PremiereDate      string            `json:"premiere_date,omitempty"`
	Genres            []string          `json:"genres,omitempty"`
	RunTimeTicks      int64             `json:"runtime_ticks,omitempty"`
	ProviderIDs       map[string]string `json:"provider_ids,omitempty"`
	Studios           []string          `json:"studios,omitempty"`
	Tagline           string            `json:"tagline,omitempty"`
	CommunityRating   float64           `json:"community_rating,omitempty"`
	CriticRating      float64           `json:"critic_rating,omitempty"`
	IndexNumber       *int              `json:"index_number,omitempty"`
	ParentIndexNumber *int              `json:"parent_index_number,omitempty"`
	SeriesID          string            `json:"series_id,omitempty"`
	SeasonID          string            `json:"season_id,omitempty"`
	// Subtitles lists the item's external subtitles, which the destination
	// copies beside its reference because Jellyfin must find them on disk.
	Subtitles       []Subtitle `json:"subtitles,omitempty"`
	HasPrimaryImage bool       `json:"has_primary_image,omitempty"`
	// File describes the media file the item streams, which a destination
	// presenting films as files needs before it reads a byte (A-17).
	File *File `json:"file,omitempty"`
}

// File is what a destination learns about an item's media file: its size,
// average bitrate, and file name extension, and nothing of its path.
type File struct {
	Size      int64  `json:"size"`
	Bitrate   int64  `json:"bitrate,omitempty"`
	Extension string `json:"extension,omitempty"`
}

// Subtitle describes one external subtitle of an item.
type Subtitle struct {
	Index         int    `json:"index"`
	Language      string `json:"language,omitempty"`
	Forced        bool   `json:"forced,omitempty"`
	Default       bool   `json:"default,omitempty"`
	MediaSourceID string `json:"media_source_id"`
}

// Catalog is this node's source catalog.
type Catalog struct {
	adapter   Adapter
	store     *store.SourceCatalogRepository
	protected map[string]bool
	pageSize  int
	audit     *audit.Log
	// mutex serializes publication changes and refreshes, so that a refresh
	// never races a publish of the same library.
	mutex sync.Mutex
}

// New returns a catalog reading Jellyfin through adapter. protected lists
// library IDs that can never be published.
func New(adapter Adapter, repository *store.SourceCatalogRepository, protected []string, log *audit.Log) *Catalog {
	set := map[string]bool{}
	for _, id := range protected {
		if id = strings.TrimSpace(id); id != "" {
			set[id] = true
		}
	}
	return &Catalog{adapter: adapter, store: repository, protected: set, pageSize: DefaultPageSize, audit: log}
}

// SetPageSize changes how many items a refresh reads at once.
func (catalog *Catalog) SetPageSize(size int) {
	if size > 0 {
		catalog.pageSize = size
	}
}

// Publish offers a library, with the root paths the operator declares for
// it, and refreshes it into the catalog.
func (catalog *Catalog) Publish(ctx context.Context, libraryID string, roots []string) error {
	catalog.mutex.Lock()
	defer catalog.mutex.Unlock()
	libraryID = strings.TrimSpace(libraryID)
	if catalog.protected[libraryID] {
		return fmt.Errorf("%w: %s", ErrProtected, libraryID)
	}
	cleaned, err := cleanRoots(roots)
	if err != nil {
		return err
	}
	library, err := catalog.visibleLibrary(ctx, libraryID)
	if err != nil {
		return err
	}
	items, err := catalog.enumerate(ctx, libraryID)
	if err != nil {
		return err
	}
	if outside := outsideRoots(items, cleaned); len(outside) > 0 {
		return fmt.Errorf("%w: %s", ErrOutsideRoots, summarize(outside))
	}
	if err := catalog.store.SavePublication(ctx, store.SourcePublication{
		LibraryID: libraryID, Name: library.Name, CollectionType: library.CollectionType, Roots: cleaned,
	}); err != nil {
		return err
	}
	_ = catalog.audit.Record(ctx, "local", "library.published", libraryID, map[string]string{"library_id": libraryID, "count": fmt.Sprint(len(items))})
	return catalog.apply(ctx, libraryID, items)
}

// Unpublish withdraws a library: its publication is removed and every item
// becomes a tombstone, which each destination applies.
func (catalog *Catalog) Unpublish(ctx context.Context, libraryID string) error {
	catalog.mutex.Lock()
	defer catalog.mutex.Unlock()
	if err := catalog.store.DeletePublication(ctx, libraryID); err != nil {
		return err
	}
	_ = catalog.audit.Record(ctx, "local", "library.unpublished", libraryID, map[string]string{"library_id": libraryID})
	return catalog.withdraw(ctx, libraryID)
}

// Refresh re-reads every live publication from Jellyfin. A library whose
// items cannot all be read is left exactly as it was, so a transient failure
// never withdraws anything. A library with an item outside its roots is
// paused and withdrawn.
func (catalog *Catalog) Refresh(ctx context.Context) error {
	catalog.mutex.Lock()
	defer catalog.mutex.Unlock()
	publications, err := catalog.store.Publications(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, publication := range publications {
		if publication.Paused {
			continue
		}
		if catalog.protected[publication.LibraryID] {
			// Protected after it was published: withdraw it at once.
			failures = append(failures, catalog.pause(ctx, publication, "the library is now protected"))
			continue
		}
		if _, err := catalog.visibleLibrary(ctx, publication.LibraryID); errors.Is(err, ErrNotVisible) {
			failures = append(failures, catalog.pause(ctx, publication, "the service user can no longer see the library"))
			continue
		} else if err != nil {
			failures = append(failures, err)
			continue
		}
		items, err := catalog.enumerate(ctx, publication.LibraryID)
		if err != nil {
			failures = append(failures, fmt.Errorf("refresh %s: %w", publication.LibraryID, err))
			continue
		}
		if outside := outsideRoots(items, publication.Roots); len(outside) > 0 {
			failures = append(failures, catalog.pause(ctx, publication, "items outside the declared roots: "+summarize(outside)))
			continue
		}
		if err := catalog.apply(ctx, publication.LibraryID, items); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (catalog *Catalog) pause(ctx context.Context, publication store.SourcePublication, reason string) error {
	publication.Paused, publication.PausedReason = true, reason
	if err := catalog.store.SavePublication(ctx, publication); err != nil {
		return err
	}
	_ = catalog.audit.Record(ctx, "local", "library.paused", publication.LibraryID, map[string]string{
		"library_id": publication.LibraryID, "reason": reason,
	})
	return catalog.withdraw(ctx, publication.LibraryID)
}

func (catalog *Catalog) visibleLibrary(ctx context.Context, libraryID string) (jellyfin.Library, error) {
	libraries, err := catalog.adapter.Libraries(ctx)
	if err != nil {
		return jellyfin.Library{}, err
	}
	for _, library := range libraries {
		if library.ID == libraryID {
			return library, nil
		}
	}
	return jellyfin.Library{}, fmt.Errorf("%w: %s", ErrNotVisible, libraryID)
}

func (catalog *Catalog) enumerate(ctx context.Context, libraryID string) ([]jellyfin.Item, error) {
	var items []jellyfin.Item
	seen := map[string]bool{}
	for start := 0; ; start += catalog.pageSize {
		page, err := catalog.adapter.Items(ctx, libraryID, start, catalog.pageSize)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if !seen[item.ID] {
				seen[item.ID] = true
				items = append(items, item)
			}
		}
		if len(page.Items) < catalog.pageSize || start+len(page.Items) >= page.Total {
			return items, nil
		}
	}
}

// typeRank orders parents before children within a refresh.
var typeRank = map[string]int{"Movie": 0, "Series": 0, "Season": 1, "Episode": 2}

// apply records every new or changed item of a library with a new sequence,
// and tombstones items no longer present once Jellyfin confirms they have
// left the library.
func (catalog *Catalog) apply(ctx context.Context, libraryID string, items []jellyfin.Item) error {
	sort.SliceStable(items, func(i, j int) bool { return typeRank[items[i].Type] < typeRank[items[j].Type] })
	present := map[string]bool{}
	for _, item := range items {
		present[item.ID] = true
		metadata, checksum, err := normalize(item)
		if err != nil {
			return err
		}
		existing, found, err := catalog.store.Item(ctx, item.ID)
		if err != nil {
			return err
		}
		if found && !existing.Tombstone && existing.LibraryID == libraryID && existing.Checksum == checksum && existing.ETag == item.ETag {
			continue
		}
		revision := uint64(1)
		if found {
			revision = existing.Revision + 1
		}
		if _, err := catalog.store.Record(ctx, store.SourceItem{
			ItemID: item.ID, LibraryID: libraryID, ParentID: parentOf(item), ItemType: item.Type,
			Revision: revision, ETag: item.ETag, Checksum: checksum, Metadata: metadata,
		}); err != nil {
			return err
		}
	}

	live, err := catalog.store.LiveItems(ctx, libraryID)
	if err != nil {
		return err
	}
	for _, row := range live {
		if present[row.ItemID] {
			continue
		}
		// Missing from this enumeration. That may be a page boundary moving
		// under concurrent edits rather than a deletion, and a false
		// tombstone would remove the item from every destination. Ask
		// Jellyfin where the item is now before withdrawing it.
		now, err := catalog.adapter.LibraryOf(ctx, row.ItemID)
		switch {
		case errors.Is(err, jellyfin.ErrNotFound), err == nil && now != libraryID:
			if err := catalog.tombstone(ctx, row); err != nil {
				return err
			}
		case err != nil:
			// Unknown: keep it until a later refresh can confirm.
		}
	}
	return nil
}

func (catalog *Catalog) withdraw(ctx context.Context, libraryID string) error {
	live, err := catalog.store.LiveItems(ctx, libraryID)
	if err != nil {
		return err
	}
	for _, row := range live {
		if err := catalog.tombstone(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

func (catalog *Catalog) tombstone(ctx context.Context, row store.SourceItem) error {
	row.Tombstone, row.Revision, row.Metadata, row.Checksum, row.ETag = true, row.Revision+1, "", "", ""
	_, err := catalog.store.Record(ctx, row)
	return err
}

func parentOf(item jellyfin.Item) string {
	switch item.Type {
	case "Season":
		return item.SeriesID
	case "Episode":
		if item.SeasonID != "" {
			return item.SeasonID
		}
		return item.SeriesID
	}
	return ""
}

func normalize(item jellyfin.Item) (string, string, error) {
	encoded, err := json.Marshal(Metadata{
		Name: item.Name, OriginalTitle: item.OriginalTitle, Overview: item.Overview, OfficialRating: item.OfficialRating,
		Year: item.ProductionYear, PremiereDate: item.PremiereDate, Genres: item.Genres, RunTimeTicks: item.RunTimeTicks,
		ProviderIDs: item.ProviderIDs, Studios: studios(item), Tagline: first(item.Taglines),
		CommunityRating: item.CommunityRating, CriticRating: item.CriticRating, IndexNumber: item.IndexNumber, ParentIndexNumber: item.ParentIndexNumber,
		SeriesID: item.SeriesID, SeasonID: item.SeasonID,
		Subtitles: externalSubtitles(item), HasPrimaryImage: item.ImageTags["Primary"] != "",
		File: mediaFile(item),
	})
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(encoded)
	return string(encoded), hex.EncodeToString(sum[:]), nil
}

func studios(item jellyfin.Item) []string {
	var names []string
	for _, studio := range item.Studios {
		if name := strings.TrimSpace(studio.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func cleanRoots(roots []string) ([]string, error) {
	var cleaned []string
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if !path.IsAbs(root) {
			return nil, fmt.Errorf("%w: %q is not absolute", ErrNoRoots, root)
		}
		if root = path.Clean(root); root == "/" {
			return nil, fmt.Errorf("%w: the filesystem root is too broad", ErrNoRoots)
		}
		cleaned = append(cleaned, root)
	}
	if len(cleaned) == 0 {
		return nil, ErrNoRoots
	}
	sort.Strings(cleaned)
	return cleaned, nil
}

// underRoot reports whether an item's path lies under one of the roots. An
// item with no path is treated as outside, since it cannot be shown to be in.
// Series and seasons are folders whose path is the folder itself.
func underRoot(itemPath string, roots []string) bool {
	if itemPath == "" {
		return false
	}
	cleaned := path.Clean(itemPath)
	for _, root := range roots {
		if cleaned == root || strings.HasPrefix(cleaned, root+"/") {
			return true
		}
	}
	return false
}

func outsideRoots(items []jellyfin.Item, roots []string) []string {
	var outside []string
	for _, item := range items {
		// Jellyfin creates a season with no folder of its own when episodes
		// sit directly in the series folder. It has no path and no media; its
		// episodes are still checked.
		if item.Type == "Season" && item.Path == "" {
			continue
		}
		if !underRoot(item.Path, roots) {
			outside = append(outside, item.ID)
		}
	}
	return outside
}

func summarize(ids []string) string {
	if len(ids) > 5 {
		return strings.Join(ids[:5], ", ") + fmt.Sprintf(" and %d more", len(ids)-5)
	}
	return strings.Join(ids, ", ")
}

// PublishedLibrary is what a destination sees of a publication.
type PublishedLibrary struct {
	LibraryID      string `json:"library_id"`
	Name           string `json:"name"`
	CollectionType string `json:"collection_type"`
	ItemCount      int    `json:"item_count"`
}

// Libraries lists the live, unpaused publications.
func (catalog *Catalog) Libraries(ctx context.Context) ([]PublishedLibrary, error) {
	publications, err := catalog.store.Publications(ctx)
	if err != nil {
		return nil, err
	}
	libraries := []PublishedLibrary{}
	for _, publication := range publications {
		if publication.Paused || catalog.protected[publication.LibraryID] {
			continue
		}
		count, err := catalog.store.CountLive(ctx, publication.LibraryID)
		if err != nil {
			return nil, err
		}
		libraries = append(libraries, PublishedLibrary{
			LibraryID: publication.LibraryID, Name: publication.Name,
			CollectionType: publication.CollectionType, ItemCount: count,
		})
	}
	return libraries, nil
}

// Publications returns every publication decision, for the operator.
func (catalog *Catalog) Publications(ctx context.Context) ([]store.SourcePublication, error) {
	return catalog.store.Publications(ctx)
}

// Change is one entry in the change stream a destination receives.
type Change struct {
	Sequence  uint64    `json:"sequence"`
	LibraryID string    `json:"library_id"`
	ItemID    string    `json:"item_id"`
	ParentID  string    `json:"parent_id,omitempty"`
	ItemType  string    `json:"item_type"`
	Revision  uint64    `json:"revision"`
	Tombstone bool      `json:"tombstone,omitempty"`
	Metadata  *Metadata `json:"metadata,omitempty"`
}

// ChangePage is one page of changes. Next is the highest sequence examined,
// which may be beyond the last change returned when changes were filtered
// out; the destination stores Next as its cursor.
type ChangePage struct {
	Changes []Change `json:"changes"`
	Next    uint64   `json:"next"`
	More    bool     `json:"more"`
}

// Changes returns changes after sequence for a destination that has opted
// out of the libraries in exclude. Nothing of an excluded library is sent,
// not even tombstones, which would reveal item identifiers the destination
// no longer holds. Live items of an unpublished, paused, or protected library
// are never sent; their tombstones are.
func (catalog *Catalog) Changes(ctx context.Context, after uint64, limit int, exclude map[string]bool) (ChangePage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	publications, err := catalog.store.Publications(ctx)
	if err != nil {
		return ChangePage{}, err
	}
	published := map[string]bool{}
	for _, publication := range publications {
		if !publication.Paused {
			published[publication.LibraryID] = true
		}
	}
	rows, err := catalog.store.Changes(ctx, after, limit)
	if err != nil {
		return ChangePage{}, err
	}
	page := ChangePage{Changes: []Change{}, Next: after, More: len(rows) == limit}
	for _, row := range rows {
		page.Next = row.Sequence
		if exclude[row.LibraryID] {
			continue
		}
		// A live row is sent only while its library is published and not
		// protected. A tombstone is sent regardless, including for a library
		// protected after it was published: the destination received those
		// items while they were published, so the tombstone reveals nothing
		// new, and without it the destination would keep them.
		if !row.Tombstone && (!published[row.LibraryID] || catalog.protected[row.LibraryID]) {
			continue
		}
		change := Change{
			Sequence: row.Sequence, LibraryID: row.LibraryID, ItemID: row.ItemID, ParentID: row.ParentID,
			ItemType: row.ItemType, Revision: row.Revision, Tombstone: row.Tombstone,
		}
		if !row.Tombstone {
			var metadata Metadata
			if err := json.Unmarshal([]byte(row.Metadata), &metadata); err != nil {
				return ChangePage{}, fmt.Errorf("decode metadata for %s: %w", row.ItemID, err)
			}
			change.Metadata = &metadata
		}
		page.Changes = append(page.Changes, change)
	}
	return page, nil
}

// Authorize checks a per-item request, such as for artwork or a stream,
// against live state: the item must be in the catalog and not withdrawn, its
// library must be published and not paused or protected, and Jellyfin must
// report the item in that library now. Jellyfin's own permissions do not gate
// the stream route (phase-0-results.md section 8), so this check cannot be
// left to the service user's restrictions.
func (catalog *Catalog) Authorize(ctx context.Context, itemID string) (store.SourceItem, error) {
	row, found, err := catalog.store.Item(ctx, itemID)
	if err != nil {
		return store.SourceItem{}, err
	}
	if !found || row.Tombstone || catalog.protected[row.LibraryID] {
		return store.SourceItem{}, ErrNotPublished
	}
	publications, err := catalog.store.Publications(ctx)
	if err != nil {
		return store.SourceItem{}, err
	}
	live := false
	for _, publication := range publications {
		if publication.LibraryID == row.LibraryID && !publication.Paused {
			live = true
		}
	}
	if !live {
		return store.SourceItem{}, ErrNotPublished
	}
	now, err := catalog.adapter.LibraryOf(ctx, itemID)
	if err != nil {
		if errors.Is(err, jellyfin.ErrNotFound) {
			return store.SourceItem{}, ErrNotPublished
		}
		return store.SourceItem{}, err
	}
	if now != row.LibraryID {
		return store.SourceItem{}, ErrNotPublished
	}
	return row, nil
}

func externalSubtitles(item jellyfin.Item) []Subtitle {
	var subtitles []Subtitle
	for _, source := range item.MediaSources {
		for _, stream := range source.MediaStreams {
			if stream.Type == "Subtitle" && stream.IsExternal {
				subtitles = append(subtitles, Subtitle{
					Index: stream.Index, Language: stream.Language, Forced: stream.IsForced,
					Default: stream.IsDefault, MediaSourceID: source.ID,
				})
			}
		}
		break // the first media source is the item's own file
	}
	return subtitles
}

// mediaFile describes the item's own file, the first media source, which the
// stream route serves. The extension comes from the path, lower-cased, and
// is sent only if it is a short alphanumeric one.
func mediaFile(item jellyfin.Item) *File {
	if len(item.MediaSources) == 0 || item.MediaSources[0].Size <= 0 {
		return nil
	}
	source := item.MediaSources[0]
	file := &File{Size: source.Size, Bitrate: source.Bitrate}
	extension := strings.ToLower(strings.TrimPrefix(path.Ext(item.Path), "."))
	if extension == "" {
		extension = strings.ToLower(strings.Split(source.Container, ",")[0])
	}
	if extensionPattern.MatchString(extension) {
		file.Extension = extension
	}
	return file
}

var extensionPattern = regexp.MustCompile(`^[a-z0-9]{1,5}$`)

// ItemMetadata returns the metadata the catalog holds for a live item, after
// authorizing it against live state.
func (catalog *Catalog) ItemMetadata(ctx context.Context, itemID string) (Metadata, error) {
	row, err := catalog.Authorize(ctx, itemID)
	if err != nil {
		return Metadata{}, err
	}
	var metadata Metadata
	if err := json.Unmarshal([]byte(row.Metadata), &metadata); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}
