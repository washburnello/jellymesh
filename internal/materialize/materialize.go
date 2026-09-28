// Package materialize writes the remote catalog into the destination's
// generated root, where Jellyfin finds it as ordinary media (design-spec
// section 9, "The generated layout", conformance.md assumption A-11).
//
// A pass is a reconciliation. It computes what should exist from the items
// this node may consume, writes whatever is missing or different, and removes
// whatever should no longer exist. Every file is written under a dot-prefixed
// temporary name and renamed into place, which Jellyfin ignores until the
// rename (conformance M-8), and NFO metadata is placed before the .strm that
// makes an item playable. Removal revokes an item's relay reference before
// its files go, and never touches a path outside the generated root.
package materialize

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"jellymesh/internal/catalogsync"
	"jellymesh/internal/sourcecatalog"
	"jellymesh/internal/store"
)

// Collection folders under the generated root. The operator adds each one to
// the matching Jellyfin library.
const (
	MoviesFolder = "Movies"
	ShowsFolder  = "TV Shows"
)

// tempPrefix marks files being written. Jellyfin ignores dot-prefixed files.
const tempPrefix = ".jellymesh-tmp-"

var ErrOutsideRoot = errors.New("path lies outside the generated root")

// Fetcher retrieves sidecar content from a source.
type Fetcher interface {
	Subtitle(ctx context.Context, sourceNodeID string, itemID string, index int) ([]byte, error)
	Image(ctx context.Context, sourceNodeID string, itemID string) ([]byte, error)
}

// Materializer writes one generated root.
type Materializer struct {
	root     string
	relayURL string
	records  *store.MaterializedRepository
	fetch    Fetcher

	// beforeRename, when set, runs just before a finished file is renamed
	// into place. Tests use it to observe that nothing is visible early.
	beforeRename func(target string)

	// OnRemove, when set, runs after an item's reference is revoked, so that
	// anything held for it elsewhere, such as the relay's cache of its first
	// bytes, can be dropped too.
	OnRemove func(store.Materialized)
}

// New returns a materializer for root, writing .strm files that name
// relayURL, such as http://127.0.0.1:8090.
func New(root string, relayURL string, records *store.MaterializedRepository, fetch Fetcher) (*Materializer, error) {
	// An empty root would resolve to the working directory, and removal
	// would then operate there.
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("a generated root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return nil, fmt.Errorf("create generated root: %w", err)
	}
	return &Materializer{root: absolute, relayURL: strings.TrimRight(relayURL, "/"), records: records, fetch: fetch}, nil
}

// Roots returns the collection folders the operator adds to Jellyfin.
func (materializer *Materializer) Roots() map[string]string {
	return map[string]string{
		"movies":  filepath.Join(materializer.root, MoviesFolder),
		"tvshows": filepath.Join(materializer.root, ShowsFolder),
	}
}

// Input is what one pass works from.
type Input struct {
	// Items are every remote item this node holds, from every source.
	Items []store.RemoteItem
	// Consumable reports whether this node may consume a source's library
	// now: the source is a member it has not blocked, and the library is
	// published and not opted out.
	Consumable func(sourceNodeID string, libraryID string) bool
	// SourceNames names each source for its version label.
	SourceNames map[string]string
}

// Result reports one pass.
type Result struct {
	Written  int
	Removed  int
	Failures []string
	// Changed are the collection folders whose contents changed, for a
	// best-effort notice to Jellyfin.
	Changed []string
}

// plan is everything written for one playable item.
type plan struct {
	item      store.RemoteItem
	metadata  sourcecatalog.Metadata
	strm      string            // relative path of the .strm
	files     map[string][]byte // relative path to content, metadata first
	subtitles map[string]int    // relative path to subtitle index
	images    map[string]imageRef
}

type imageRef struct{ source, item string }

// Reconcile brings the generated root into line with input.
func (materializer *Materializer) Reconcile(ctx context.Context, input Input) (Result, error) {
	var result Result
	plans, err := materializer.plan(input)
	if err != nil {
		return result, err
	}
	existing, err := materializer.records.All(ctx)
	if err != nil {
		return result, err
	}
	held := map[string]store.Materialized{}
	for _, record := range existing {
		held[record.SourceNodeID+"/"+record.ItemID] = record
	}
	changed := map[string]bool{}

	// Removals first, so a renamed item does not briefly exist twice.
	for key, record := range held {
		planned, keep := planFor(plans, key)
		if keep && planned.strm == record.Path {
			continue
		}
		if err := materializer.remove(ctx, record); err != nil {
			result.Failures = append(result.Failures, err.Error())
			continue
		}
		delete(held, key)
		result.Removed++
		changed[topFolder(record.Path)] = true
	}

	for _, plan := range plans {
		key := plan.item.SourceNodeID + "/" + plan.item.ItemID
		record, known := held[key]
		reference := record.Reference
		if !known {
			if reference, err = store.NewReference(); err != nil {
				return result, err
			}
		}
		written, err := materializer.write(ctx, plan, reference)
		if err != nil {
			result.Failures = append(result.Failures, fmt.Sprintf("%s: %v", plan.strm, err))
			continue
		}
		if written > 0 || !known {
			result.Written++
			changed[topFolder(plan.strm)] = true
		}
		if !known || record.Path != plan.strm || record.Revision != plan.item.Revision {
			if err := materializer.records.Save(ctx, store.Materialized{
				SourceNodeID: plan.item.SourceNodeID, ItemID: plan.item.ItemID, LibraryID: plan.item.LibraryID,
				Reference: reference, Path: plan.strm, Checksum: checksum(plan), Revision: plan.item.Revision,
			}); err != nil {
				return result, err
			}
		}
	}
	for folder := range changed {
		result.Changed = append(result.Changed, filepath.Join(materializer.root, folder))
	}
	sort.Strings(result.Changed)
	return result, nil
}

func planFor(plans []plan, key string) (plan, bool) {
	for _, candidate := range plans {
		if candidate.item.SourceNodeID+"/"+candidate.item.ItemID == key {
			return candidate, true
		}
	}
	return plan{}, false
}

func topFolder(relative string) string {
	return strings.SplitN(filepath.ToSlash(relative), "/", 2)[0]
}

// plan computes what should exist.
func (materializer *Materializer) plan(input Input) ([]plan, error) {
	byID := map[string]store.RemoteItem{}
	for _, item := range input.Items {
		byID[item.SourceNodeID+"/"+item.ItemID] = item
	}
	names := labels(input.SourceNames)

	// Movies sharing a strong identity share a folder, so gather them first.
	type version struct {
		item     store.RemoteItem
		metadata sourcecatalog.Metadata
	}
	works := map[string][]version{}
	var plans []plan
	for _, item := range input.Items {
		if input.Consumable != nil && !input.Consumable(item.SourceNodeID, item.LibraryID) {
			continue
		}
		var metadata sourcecatalog.Metadata
		if err := json.Unmarshal([]byte(item.Metadata), &metadata); err != nil {
			continue
		}
		switch item.ItemType {
		case "Movie":
			key := catalogsync.LogicalWorkID("Movie", item.Metadata)
			if key == "" {
				key = "source:" + item.SourceNodeID + ":" + item.ItemID
			}
			works[key] = append(works[key], version{item, metadata})
		case "Episode":
			if episode, ok := materializer.planEpisode(item, metadata, byID); ok {
				plans = append(plans, episode)
			}
		}
	}

	keys := make([]string, 0, len(works))
	for key := range works {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		versions := works[key]
		sort.Slice(versions, func(i, j int) bool { return versions[i].item.SourceNodeID < versions[j].item.SourceNodeID })
		// The folder and its NFO follow the first source, deterministically.
		lead := versions[0].metadata
		folder := filepath.Join(MoviesFolder, folderName(lead.Name, lead.Year, key))
		nfo := movieNFO(lead)
		var poster *imageRef
		for _, candidate := range versions {
			if candidate.metadata.HasPrimaryImage {
				poster = &imageRef{candidate.item.SourceNodeID, candidate.item.ItemID}
				break
			}
		}
		for _, candidate := range versions {
			base := filepath.Base(folder) + " - " + names[candidate.item.SourceNodeID]
			current := plan{
				item: candidate.item, metadata: candidate.metadata,
				strm:      filepath.Join(folder, base+".strm"),
				files:     map[string][]byte{filepath.Join(folder, "movie.nfo"): nfo},
				subtitles: subtitleFiles(folder, base, candidate.metadata.Subtitles),
				images:    map[string]imageRef{},
			}
			if poster != nil {
				current.images[filepath.Join(folder, "poster.jpg")] = *poster
			}
			plans = append(plans, current)
		}
	}
	return plans, nil
}

// planEpisode places an episode under its source's own series folder. Series
// are not merged across sources yet (A-11): that is Phase 4.
func (materializer *Materializer) planEpisode(item store.RemoteItem, metadata sourcecatalog.Metadata, byID map[string]store.RemoteItem) (plan, bool) {
	seriesItem, ok := byID[item.SourceNodeID+"/"+metadata.SeriesID]
	if !ok {
		return plan{}, false // the series arrives before its episodes; wait for it
	}
	var series sourcecatalog.Metadata
	if json.Unmarshal([]byte(seriesItem.Metadata), &series) != nil {
		return plan{}, false
	}
	seasonNumber, episodeNumber := 0, 0
	if metadata.ParentIndexNumber != nil {
		seasonNumber = *metadata.ParentIndexNumber
	}
	if metadata.IndexNumber != nil {
		episodeNumber = *metadata.IndexNumber
	}
	seriesFolder := filepath.Join(ShowsFolder, folderName(series.Name, series.Year, "series:"+item.SourceNodeID+":"+seriesItem.ItemID))
	seasonFolder := filepath.Join(seriesFolder, fmt.Sprintf("Season %02d", seasonNumber))
	base := fmt.Sprintf("%s S%02dE%02d", sanitize(series.Name), seasonNumber, episodeNumber)
	current := plan{
		item: item, metadata: metadata,
		strm: filepath.Join(seasonFolder, base+".strm"),
		files: map[string][]byte{
			filepath.Join(seriesFolder, "tvshow.nfo"): showNFO(series),
			filepath.Join(seasonFolder, base+".nfo"):  episodeNFO(metadata, seasonNumber, episodeNumber),
		},
		subtitles: subtitleFiles(seasonFolder, base, metadata.Subtitles),
		images:    map[string]imageRef{},
	}
	if series.HasPrimaryImage {
		current.images[filepath.Join(seriesFolder, "poster.jpg")] = imageRef{item.SourceNodeID, seriesItem.ItemID}
	}
	return current, true
}

// labels turns source names into version labels, distinct from one another.
func labels(names map[string]string) map[string]string {
	counts := map[string]int{}
	cleaned := map[string]string{}
	for id, name := range names {
		label := sanitize(name)
		if label == "Untitled" {
			label = "Source " + short(id)
		}
		cleaned[id] = label
		counts[strings.ToLower(label)]++
	}
	for id, label := range cleaned {
		if counts[strings.ToLower(label)] > 1 {
			cleaned[id] = label + " (" + short(id) + ")"
		}
	}
	return cleaned
}

func short(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// folderName is "<Title> (<Year>) [jmid-<id>]". The id is derived from the
// work's identity so that it is stable, but it is Jellymesh's own and carries
// no provider identifier (A-2).
func folderName(title string, year int, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	name := sanitize(title)
	// A title that already ends with its year, as Jellyfin reports when it
	// named the item from a "Title (Year)" folder, keeps just the one.
	if year > 0 && !strings.HasSuffix(name, "("+strconv.Itoa(year)+")") {
		name += " (" + strconv.Itoa(year) + ")"
	}
	return name + " [jmid-" + hex.EncodeToString(sum[:])[:10] + "]"
}

// sanitize makes a name safe as one path component on every filesystem
// Jellyfin runs on.
func sanitize(name string) string {
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			builder.WriteRune(' ')
		case strings.ContainsRune(`/\:*?"<>|[]`, r):
			builder.WriteRune(' ')
		default:
			builder.WriteRune(r)
		}
	}
	cleaned := strings.Join(strings.Fields(builder.String()), " ")
	cleaned = strings.Trim(cleaned, ". ")
	if len(cleaned) > 120 {
		cleaned = strings.TrimSpace(cleaned[:120])
	}
	if cleaned == "" {
		return "Untitled"
	}
	return cleaned
}

func subtitleFiles(folder string, base string, subtitles []sourcecatalog.Subtitle) map[string]int {
	files := map[string]int{}
	used := map[string]bool{}
	for _, subtitle := range subtitles {
		language := strings.ToLower(sanitize(subtitle.Language))
		if language == "untitled" {
			language = "und"
		}
		name := base + "." + language
		if subtitle.Forced {
			name += ".forced"
		}
		if used[name] {
			name += "." + strconv.Itoa(subtitle.Index)
		}
		used[name] = true
		files[filepath.Join(folder, name+".srt")] = subtitle.Index
	}
	return files
}

// write puts one item's files in place, metadata and sidecars first and the
// .strm last. It returns how many files it wrote.
func (materializer *Materializer) write(ctx context.Context, plan plan, reference string) (int, error) {
	written := 0
	paths := make([]string, 0, len(plan.files))
	for path := range plan.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		did, err := materializer.writeFile(path, plan.files[path])
		if err != nil {
			return written, err
		}
		written += did
	}
	for path, index := range plan.subtitles {
		if materializer.exists(path) || materializer.fetch == nil {
			continue
		}
		content, err := materializer.fetch.Subtitle(ctx, plan.item.SourceNodeID, plan.item.ItemID, index)
		if err != nil {
			continue // retried on the next pass
		}
		did, err := materializer.writeFile(path, content)
		if err != nil {
			return written, err
		}
		written += did
	}
	for path, image := range plan.images {
		if materializer.exists(path) || materializer.fetch == nil {
			continue
		}
		content, err := materializer.fetch.Image(ctx, image.source, image.item)
		if err != nil {
			continue
		}
		did, err := materializer.writeFile(path, content)
		if err != nil {
			return written, err
		}
		written += did
	}
	did, err := materializer.writeFile(plan.strm, []byte(materializer.relayURL+"/r/"+reference+"\n"))
	return written + did, err
}

func (materializer *Materializer) exists(relative string) bool {
	target, err := materializer.resolve(relative)
	if err != nil {
		return false
	}
	_, err = os.Stat(target)
	return err == nil
}

// resolve turns a relative path into an absolute one, refusing anything that
// would land outside the generated root.
func (materializer *Materializer) resolve(relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, relative)
	}
	target := filepath.Join(materializer.root, relative)
	within, err := filepath.Rel(materializer.root, target)
	if err != nil || within == "." || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, relative)
	}
	return target, nil
}

// writeFile writes content atomically, unless the file already holds it. It
// returns 1 if it wrote.
func (materializer *Materializer) writeFile(relative string, content []byte) (int, error) {
	target, err := materializer.resolve(relative)
	if err != nil {
		return 0, err
	}
	if current, err := os.ReadFile(target); err == nil && bytes.Equal(current, content) {
		return 0, nil
	}
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return 0, err
	}
	temporary, err := os.CreateTemp(directory, tempPrefix+"*")
	if err != nil {
		return 0, err
	}
	name := temporary.Name()
	defer os.Remove(name) // a no-op once renamed
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return 0, err
	}
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return 0, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return 0, err
	}
	if err := temporary.Close(); err != nil {
		return 0, err
	}
	if materializer.beforeRename != nil {
		materializer.beforeRename(target)
	}
	if err := os.Rename(name, target); err != nil {
		return 0, err
	}
	return 1, nil
}

// remove withdraws one item: its reference is revoked first, so it stops
// being playable before any file goes, then its .strm and sidecars are
// deleted, then any folder left without a playable item.
func (materializer *Materializer) remove(ctx context.Context, record store.Materialized) error {
	target, err := materializer.resolve(record.Path)
	if err != nil || filepath.Ext(target) != ".strm" {
		return fmt.Errorf("refusing to remove %q: %w", record.Path, ErrOutsideRoot)
	}
	if err := materializer.records.Remove(ctx, record.SourceNodeID, record.ItemID); err != nil {
		return err
	}
	if materializer.OnRemove != nil {
		materializer.OnRemove(record)
	}
	directory := filepath.Dir(target)
	base := strings.TrimSuffix(filepath.Base(target), ".strm")
	entries, _ := os.ReadDir(directory)
	for _, entry := range entries {
		name := entry.Name()
		if name == base+".strm" || strings.HasPrefix(name, base+".") {
			os.Remove(filepath.Join(directory, name))
		}
	}
	materializer.pruneEmpty(directory)
	return nil
}

// pruneEmpty removes folders, upwards towards the collection folder, that
// hold no .strm anywhere beneath them. Their remaining files are generated
// metadata and artwork.
func (materializer *Materializer) pruneEmpty(directory string) {
	for {
		within, err := filepath.Rel(materializer.root, directory)
		if err != nil || within == "." || !strings.Contains(within, string(filepath.Separator)) {
			return // never the root or a collection folder
		}
		if holdsPlayable(directory) {
			return
		}
		if err := os.RemoveAll(directory); err != nil {
			return
		}
		directory = filepath.Dir(directory)
	}
}

func holdsPlayable(directory string) bool {
	found := false
	filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && filepath.Ext(path) == ".strm" && !strings.HasPrefix(entry.Name(), ".") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func checksum(plan plan) string {
	hash := sha256.New()
	paths := make([]string, 0, len(plan.files))
	for path := range plan.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		hash.Write([]byte(path))
		hash.Write(plan.files[path])
	}
	hash.Write([]byte(plan.strm))
	return hex.EncodeToString(hash.Sum(nil))
}

// NFO documents. Provider identifiers are written both as <uniqueid> and as
// the legacy per-provider elements, because on 10.11.11 only the legacy ones
// took effect (conformance M-8). Provider identifiers never appear in a path.

type uniqueID struct {
	Type    string `xml:"type,attr"`
	Default bool   `xml:"default,attr,omitempty"`
	Value   string `xml:",chardata"`
}

type movieDocument struct {
	XMLName       xml.Name   `xml:"movie"`
	Title         string     `xml:"title"`
	OriginalTitle string     `xml:"originaltitle,omitempty"`
	Year          int        `xml:"year,omitempty"`
	Plot          string     `xml:"plot,omitempty"`
	MPAA          string     `xml:"mpaa,omitempty"`
	Premiered     string     `xml:"premiered,omitempty"`
	Runtime       int64      `xml:"runtime,omitempty"`
	Genres        []string   `xml:"genre"`
	UniqueIDs     []uniqueID `xml:"uniqueid"`
	TMDB          string     `xml:"tmdbid,omitempty"`
	IMDB          string     `xml:"imdbid,omitempty"`
	TVDB          string     `xml:"tvdbid,omitempty"`
	LockData      bool       `xml:"lockdata"`
}

type showDocument struct {
	XMLName   xml.Name   `xml:"tvshow"`
	Title     string     `xml:"title"`
	Year      int        `xml:"year,omitempty"`
	Plot      string     `xml:"plot,omitempty"`
	Genres    []string   `xml:"genre"`
	UniqueIDs []uniqueID `xml:"uniqueid"`
	TMDB      string     `xml:"tmdbid,omitempty"`
	IMDB      string     `xml:"imdbid,omitempty"`
	TVDB      string     `xml:"tvdbid,omitempty"`
	LockData  bool       `xml:"lockdata"`
}

type episodeDocument struct {
	XMLName   xml.Name   `xml:"episodedetails"`
	Title     string     `xml:"title"`
	Season    int        `xml:"season"`
	Episode   int        `xml:"episode"`
	Aired     string     `xml:"aired,omitempty"`
	Plot      string     `xml:"plot,omitempty"`
	Runtime   int64      `xml:"runtime,omitempty"`
	UniqueIDs []uniqueID `xml:"uniqueid"`
	LockData  bool       `xml:"lockdata"`
}

func providers(ids map[string]string) (unique []uniqueID, tmdb, imdb, tvdb string) {
	keys := make([]string, 0, len(ids))
	for key := range ids {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := strings.TrimSpace(ids[key])
		if value == "" {
			continue
		}
		switch strings.ToLower(key) {
		case "tmdb":
			tmdb = value
		case "imdb":
			imdb = value
		case "tvdb":
			tvdb = value
		}
		unique = append(unique, uniqueID{Type: strings.ToLower(key), Value: value})
	}
	if len(unique) > 0 {
		unique[0].Default = true
	}
	return
}

func minutes(ticks int64) int64 { return ticks / 600_000_000 }

func date(value string) string {
	if len(value) >= 10 {
		return value[:10]
	}
	return value
}

func encode(document any) []byte {
	encoded, _ := xml.MarshalIndent(document, "", "  ")
	return append([]byte(xml.Header), append(encoded, '\n')...)
}

func movieNFO(metadata sourcecatalog.Metadata) []byte {
	unique, tmdb, imdb, tvdb := providers(metadata.ProviderIDs)
	return encode(movieDocument{
		Title: metadata.Name, OriginalTitle: metadata.OriginalTitle, Year: metadata.Year, Plot: metadata.Overview,
		MPAA: metadata.OfficialRating, Premiered: date(metadata.PremiereDate), Runtime: minutes(metadata.RunTimeTicks),
		Genres: metadata.Genres, UniqueIDs: unique, TMDB: tmdb, IMDB: imdb, TVDB: tvdb, LockData: true,
	})
}

func showNFO(metadata sourcecatalog.Metadata) []byte {
	unique, tmdb, imdb, tvdb := providers(metadata.ProviderIDs)
	return encode(showDocument{
		Title: metadata.Name, Year: metadata.Year, Plot: metadata.Overview, Genres: metadata.Genres,
		UniqueIDs: unique, TMDB: tmdb, IMDB: imdb, TVDB: tvdb, LockData: true,
	})
}

func episodeNFO(metadata sourcecatalog.Metadata, season int, episode int) []byte {
	unique, _, _, _ := providers(metadata.ProviderIDs)
	return encode(episodeDocument{
		Title: metadata.Name, Season: season, Episode: episode, Aired: date(metadata.PremiereDate),
		Plot: metadata.Overview, Runtime: minutes(metadata.RunTimeTicks), UniqueIDs: unique, LockData: true,
	})
}
