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
	"time"

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
	pins     *store.WorkPinRepository
	fetch    Fetcher

	// now is the clock, which tests replace.
	now func() time.Time

	// beforeRename, when set, runs just before a finished file is renamed
	// into place. Tests use it to observe that nothing is visible early.
	beforeRename func(target string)

	// OnRemove, when set, runs after an item's reference is revoked, so that
	// anything held for it elsewhere, such as the relay's cache of its first
	// bytes, can be dropped too.
	OnRemove func(store.Materialized)
}

// PinRetention is how long a work keeps its folder and identifiers after it
// was last materialized. Jellyfin keeps a removed item's user state for 90
// days before its clean-up task may delete it (A-14).
const PinRetention = 90 * 24 * time.Hour

// New returns a materializer for root, writing .strm files that name
// relayURL, such as http://127.0.0.1:8090.
func New(root string, relayURL string, records *store.MaterializedRepository, pins *store.WorkPinRepository, fetch Fetcher) (*Materializer, error) {
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
	return &Materializer{root: absolute, relayURL: strings.TrimRight(relayURL, "/"), records: records, pins: pins, fetch: fetch, now: time.Now}, nil
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
	existing, err := materializer.records.All(ctx)
	if err != nil {
		return result, err
	}
	held := map[string]store.Materialized{}
	for _, record := range existing {
		held[record.SourceNodeID+"/"+record.ItemID] = record
	}
	now := materializer.now()
	if _, err := materializer.pins.Prune(ctx, now.Add(-PinRetention)); err != nil {
		return result, err
	}
	pins, err := materializer.pins.All(ctx)
	if err != nil {
		return result, err
	}
	plans, pinning := materializer.plan(input, held, pins)
	for _, pin := range pinning.added {
		pin.LastUsed = now
		if _, err := materializer.pins.Add(ctx, pin); err != nil {
			return result, err
		}
	}
	if err := materializer.pins.Touch(ctx, pinning.used, now); err != nil {
		return result, err
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
			// A path that held a film keeps its reference for what comes
			// next, so the .strm Jellyfin already read stays valid (#61).
			if reference, err = materializer.records.ReferenceFor(ctx, plan.strm); err != nil {
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

// candidate is a consumable item with its decoded metadata.
type candidate struct {
	item     store.RemoteItem
	metadata sourcecatalog.Metadata
}

func (c candidate) key() string { return c.item.SourceNodeID + "/" + c.item.ItemID }

// pinning is what a plan did with work pins.
type pinning struct {
	used  []int64
	added []store.WorkPin
	taken map[string]bool // folders held by any pin
}

// plan computes what should exist. held are the items already materialized,
// which keep their place when a choice between sources is open, and pins are
// the folders and identifiers works were first materialized under.
func (materializer *Materializer) plan(input Input, held map[string]store.Materialized, pins []store.WorkPin) ([]plan, pinning) {
	names := labels(input.SourceNames)
	var movies, series, episodes []candidate
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
			movies = append(movies, candidate{item, metadata})
		case "Series":
			series = append(series, candidate{item, metadata})
		case "Episode":
			episodes = append(episodes, candidate{item, metadata})
		}
	}
	pinned := pinning{taken: map[string]bool{}}
	for _, pin := range pins {
		pinned.taken[pin.Folder] = true
	}
	used := map[string]bool{}
	plans := materializer.planMovies(group(movies, pins, "movie"), names, used, &pinned)
	return append(plans, materializer.planEpisodes(group(series, pins, "series"), episodes, names, held, used, &pinned)...), pinned
}

// grouped is one work: its items in key order, and its pin, if it has one.
type grouped struct {
	work
	items []candidate
	pin   *store.WorkPin
}

// group sorts candidates into works. Pins of the kind take part as members,
// so that a pin joins the items it was made for even when they no longer
// share an identifier among themselves; a work that holds only pins is
// nothing to materialize. A work with several pins keeps the oldest.
func group(candidates []candidate, pins []store.WorkPin, kind string) []grouped {
	byKey := map[string]candidate{}
	pinByKey := map[string]store.WorkPin{}
	members := make([]member, 0, len(candidates)+len(pins))
	for _, c := range candidates {
		byKey[c.key()] = c
		members = append(members, member{key: c.key(), ids: strongIDs(c.metadata.ProviderIDs)})
	}
	for _, pin := range pins {
		if pin.Kind != kind || len(pin.Identifiers) == 0 {
			continue
		}
		// "\x00" sorts before any item key, and the zero-padded ID keeps
		// pins in age order among themselves.
		key := fmt.Sprintf("\x00pin:%020d", pin.ID)
		pinByKey[key] = pin
		members = append(members, member{key: key, ids: pin.Identifiers})
	}
	var works []grouped
	for _, w := range groupWorks(members) {
		current := grouped{work: work{ids: w.ids}}
		for _, key := range w.members {
			if c, ok := byKey[key]; ok {
				current.items = append(current.items, c)
				current.members = append(current.members, key)
			} else if pin, ok := pinByKey[key]; ok && current.pin == nil {
				current.pin = &pin
			}
		}
		if len(current.items) > 0 {
			works = append(works, current)
		}
	}
	return works
}

// folder returns a work's folder under collection: its pin's, or a new one
// derived from its strongest identifier, which is then pinned. A work with
// no strong identifier holds one source item, and its folder derives from
// that item and is not pinned, since nothing could return to it.
func (pinned *pinning) folder(w grouped, kind string, collection string, name string, year int) string {
	if w.pin != nil {
		pinned.used = append(pinned.used, w.pin.ID)
		return w.pin.Folder
	}
	lead := w.items[0]
	if len(w.ids) == 0 {
		return filepath.Join(collection, folderName(name, year, "source:"+lead.item.SourceNodeID+":"+lead.item.ItemID))
	}
	folder := filepath.Join(collection, folderName(name, year, w.identity(kind)))
	// Another work, which disagrees with this one, may have been pinned
	// here; derive another folder until one is free.
	encoded, _ := json.Marshal(w.ids)
	for attempt := 0; pinned.taken[folder]; attempt++ {
		folder = filepath.Join(collection, folderName(name, year, kind+":"+string(encoded)+":"+strconv.Itoa(attempt)))
	}
	pinned.taken[folder] = true
	ids := map[string]string{}
	for provider, value := range w.ids {
		ids[provider] = value
	}
	pinned.added = append(pinned.added, store.WorkPin{Kind: kind, Folder: folder, Identifiers: ids})
	return folder
}

// seriesIdentifiers are what a show's NFO carries. Jellyfin keys episode
// state under the series' preferred identifier, TVDB before TMDB, so a show
// that gained a TVDB identifier after its episodes were watched would lose
// that state (conformance M-10). A pinned show therefore keeps exactly the
// identifiers it was pinned with.
func seriesIdentifiers(w grouped) map[string]string {
	if w.pin != nil {
		return w.pin.Identifiers
	}
	return w.ids
}

// withIdentifiers is metadata carrying every identifier of its work, so that
// Jellyfin keys the one item it makes under all of them.
func withIdentifiers(metadata sourcecatalog.Metadata, ids map[string]string) sourcecatalog.Metadata {
	merged := map[string]string{}
	for key, value := range metadata.ProviderIDs {
		if _, strong := ids[strings.ToLower(key)]; !strong {
			merged[key] = value
		}
	}
	for provider, value := range ids {
		merged[provider] = value
	}
	metadata.ProviderIDs = merged
	return metadata
}

// planMovies lays out one folder per work, with one version per source item.
func (materializer *Materializer) planMovies(works []grouped, names map[string]string, used map[string]bool, pinned *pinning) []plan {
	var plans []plan
	for _, w := range works {
		versions := w.items
		// The folder and its NFO follow the first source, deterministically.
		lead := versions[0]
		folder := pinned.folder(w, "movie", MoviesFolder, lead.metadata.Name, lead.metadata.Year)
		sources := make([]string, 0, len(versions))
		for _, version := range versions {
			sources = append(sources, version.item.SourceNodeID)
		}
		nfo := movieNFO(withIdentifiers(lead.metadata, w.ids), sourceTags(names, sources...))
		var poster *imageRef
		for _, version := range versions {
			if version.metadata.HasPrimaryImage {
				poster = &imageRef{version.item.SourceNodeID, version.item.ItemID}
				break
			}
		}
		// A source holding two copies of one work labels them apart.
		perSource := map[string]int{}
		for _, version := range versions {
			perSource[version.item.SourceNodeID]++
			label := names[version.item.SourceNodeID]
			if perSource[version.item.SourceNodeID] > 1 {
				label += " " + strconv.Itoa(perSource[version.item.SourceNodeID])
			}
			base := filepath.Base(folder) + " - " + label
			current := plan{
				item: version.item, metadata: version.metadata,
				strm:      unique(filepath.Join(folder, base), version.key(), used),
				files:     map[string][]byte{filepath.Join(folder, "movie.nfo"): nfo},
				subtitles: map[string]int{},
				images:    map[string]imageRef{},
			}
			stem := strings.TrimSuffix(current.strm, ".strm")
			current.subtitles = subtitleFiles(filepath.Dir(stem), filepath.Base(stem), version.metadata.Subtitles)
			if poster != nil {
				current.images[filepath.Join(folder, "poster.jpg")] = *poster
			}
			plans = append(plans, current)
		}
	}
	return plans
}

// planEpisodes groups series across sources as works, and gives each episode
// of a work one file. Jellyfin does not show episode files from several
// sources as versions of one episode (conformance M-10), so one source is
// chosen per episode: the one already materialized while it remains, and
// otherwise the first by source and item (A-13).
func (materializer *Materializer) planEpisodes(works []grouped, episodes []candidate, names map[string]string, held map[string]store.Materialized, used map[string]bool, pinned *pinning) []plan {
	workOf := map[string]int{}
	for index, w := range works {
		for _, key := range w.members {
			workOf[key] = index
		}
	}

	type slot struct {
		work       int
		numbered   bool
		season     int
		episode    int
		candidates []candidate
	}
	slots := map[string]*slot{}
	for _, episode := range episodes {
		index, ok := workOf[episode.item.SourceNodeID+"/"+episode.metadata.SeriesID]
		if !ok {
			continue // the series arrives before its episodes; wait for it
		}
		current := slot{work: index}
		id := episode.key()
		if episode.metadata.ParentIndexNumber != nil && episode.metadata.IndexNumber != nil {
			current.numbered, current.season, current.episode = true, *episode.metadata.ParentIndexNumber, *episode.metadata.IndexNumber
			id = fmt.Sprintf("%d/%d/%d", index, current.season, current.episode)
		} else if episode.metadata.ParentIndexNumber != nil {
			current.season = *episode.metadata.ParentIndexNumber
		}
		if slots[id] == nil {
			slots[id] = &current
		}
		slots[id].candidates = append(slots[id].candidates, episode)
	}
	ids := make([]string, 0, len(slots))
	for id := range slots {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// A show is tagged with the sources its episodes play from.
	playing := map[int][]string{}
	chosen := map[string]candidate{}
	for _, id := range ids {
		current := slots[id]
		chosen[id] = choose(current.candidates, held)
		playing[current.work] = append(playing[current.work], chosen[id].item.SourceNodeID)
	}
	showTags := map[int][]string{}
	for work, sources := range playing {
		showTags[work] = sourceTags(names, sources...)
	}

	folders := map[int]string{}
	var plans []plan
	for _, id := range ids {
		current := slots[id]
		chosen := chosen[id]
		w := works[current.work]
		lead := w.items[0]
		seriesFolder, ok := folders[current.work]
		if !ok {
			seriesFolder = pinned.folder(w, "series", ShowsFolder, lead.metadata.Name, lead.metadata.Year)
			folders[current.work] = seriesFolder
		}
		seasonFolder := filepath.Join(seriesFolder, fmt.Sprintf("Season %02d", current.season))
		base := fmt.Sprintf("%s S%02dE%02d", sanitize(lead.metadata.Name), current.season, current.episode)
		if !current.numbered {
			base = sanitize(lead.metadata.Name) + " - " + sanitize(chosen.metadata.Name)
		}
		strm := unique(filepath.Join(seasonFolder, base), chosen.key(), used)
		stem := strings.TrimSuffix(strm, ".strm")
		next := plan{
			item: chosen.item, metadata: chosen.metadata,
			strm: strm,
			files: map[string][]byte{
				filepath.Join(seriesFolder, "tvshow.nfo"): showNFO(withIdentifiers(lead.metadata, seriesIdentifiers(w)), showTags[current.work]),
				stem + ".nfo": episodeNFO(chosen.metadata, current.season, current.episode, sourceTags(names, chosen.item.SourceNodeID)),
			},
			subtitles: subtitleFiles(seasonFolder, filepath.Base(stem), chosen.metadata.Subtitles),
			images:    map[string]imageRef{},
		}
		for _, member := range w.items {
			if member.metadata.HasPrimaryImage {
				next.images[filepath.Join(seriesFolder, "poster.jpg")] = imageRef{member.item.SourceNodeID, member.item.ItemID}
				break
			}
		}
		plans = append(plans, next)
	}
	return plans
}

// choose picks the item that plays an episode: the one already materialized
// if it is still a candidate, so that a new source does not displace it, and
// otherwise the first by source and item.
func choose(candidates []candidate, held map[string]store.Materialized) candidate {
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].key() < candidates[j].key() })
	for _, c := range candidates {
		if _, ok := held[c.key()]; ok {
			return c
		}
	}
	return candidates[0]
}

// unique returns stem + ".strm", distinguished by the item it plays if
// another plan already took that path.
func unique(stem string, key string, used map[string]bool) string {
	path := stem + ".strm"
	if used[path] {
		sum := sha256.Sum256([]byte(key))
		path = stem + " " + hex.EncodeToString(sum[:])[:6] + ".strm"
	}
	used[path] = true
	return path
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
	Tagline       string     `xml:"tagline,omitempty"`
	Rating        float64    `xml:"rating,omitempty"`
	CriticRating  float64    `xml:"criticrating,omitempty"`
	Genres        []string   `xml:"genre"`
	Studios       []string   `xml:"studio"`
	Tags          []string   `xml:"tag"`
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
	Rating    float64    `xml:"rating,omitempty"`
	Genres    []string   `xml:"genre"`
	Studios   []string   `xml:"studio"`
	Tags      []string   `xml:"tag"`
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
	Rating    float64    `xml:"rating,omitempty"`
	Tags      []string   `xml:"tag"`
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

// sourceTags name the sources an item comes from, as Jellyfin tags, which
// its item page shows and its library filters offer. They label a source
// where a version label cannot, as for an episode, which has one file.
func sourceTags(names map[string]string, sources ...string) []string {
	seen := map[string]bool{}
	var tags []string
	for _, source := range sources {
		tag := "From " + names[source]
		if !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	return tags
}

func movieNFO(metadata sourcecatalog.Metadata, tags []string) []byte {
	unique, tmdb, imdb, tvdb := providers(metadata.ProviderIDs)
	return encode(movieDocument{
		Title: metadata.Name, OriginalTitle: metadata.OriginalTitle, Year: metadata.Year, Plot: metadata.Overview,
		Tagline: metadata.Tagline, Rating: metadata.CommunityRating, CriticRating: metadata.CriticRating,
		MPAA: metadata.OfficialRating, Premiered: date(metadata.PremiereDate), Runtime: minutes(metadata.RunTimeTicks),
		Genres: metadata.Genres, Studios: metadata.Studios, Tags: tags,
		UniqueIDs: unique, TMDB: tmdb, IMDB: imdb, TVDB: tvdb, LockData: true,
	})
}

func showNFO(metadata sourcecatalog.Metadata, tags []string) []byte {
	unique, tmdb, imdb, tvdb := providers(metadata.ProviderIDs)
	return encode(showDocument{
		Title: metadata.Name, Year: metadata.Year, Plot: metadata.Overview, Rating: metadata.CommunityRating,
		Genres: metadata.Genres, Studios: metadata.Studios, Tags: tags,
		UniqueIDs: unique, TMDB: tmdb, IMDB: imdb, TVDB: tvdb, LockData: true,
	})
}

func episodeNFO(metadata sourcecatalog.Metadata, season int, episode int, tags []string) []byte {
	unique, _, _, _ := providers(metadata.ProviderIDs)
	return encode(episodeDocument{
		Title: metadata.Name, Season: season, Episode: episode, Aired: date(metadata.PremiereDate),
		Plot: metadata.Overview, Runtime: minutes(metadata.RunTimeTicks), Rating: metadata.CommunityRating,
		Tags: tags, UniqueIDs: unique, LockData: true,
	})
}
