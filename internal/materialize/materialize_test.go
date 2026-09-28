package materialize

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"jellymesh/internal/sourcecatalog"
	"jellymesh/internal/store"
)

const relayURL = "http://127.0.0.1:8090"

type fetcher struct {
	fail  bool
	calls int
}

func (f *fetcher) Subtitle(_ context.Context, source string, item string, index int) ([]byte, error) {
	f.calls++
	if f.fail {
		return nil, errors.New("source unreachable")
	}
	return []byte("1\n00:00:01,000 --> 00:00:02,000\n" + source + "/" + item + "\n"), nil
}

func (f *fetcher) Image(_ context.Context, source string, item string) ([]byte, error) {
	f.calls++
	if f.fail {
		return nil, errors.New("source unreachable")
	}
	return []byte("image of " + item), nil
}

type fixture struct {
	root     string
	outside  string
	records  *store.MaterializedRepository
	fetcher  *fetcher
	material *Materializer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	database, err := store.Open(filepath.Join(base, "state", "jellymesh.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	f := &fixture{root: filepath.Join(base, "generated"), outside: filepath.Join(base, "precious"), records: store.NewMaterializedRepository(database), fetcher: &fetcher{}}
	if f.material, err = New(f.root, relayURL, f.records, f.fetcher); err != nil {
		t.Fatalf("new: %v", err)
	}
	os.MkdirAll(f.outside, 0o755)
	os.WriteFile(filepath.Join(f.outside, "keep.strm"), []byte("not ours"), 0o644)
	return f
}

func item(t *testing.T, source string, id string, library string, kind string, metadata sourcecatalog.Metadata) store.RemoteItem {
	t.Helper()
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return store.RemoteItem{SourceNodeID: source, ItemID: id, LibraryID: library, ItemType: kind, Revision: 1, Metadata: string(encoded)}
}

func intPointer(value int) *int { return &value }

var names = map[string]string{"cedar": "Cedar", "walnut": "Walnut"}

func everything(string, string) bool { return true }

func (f *fixture) reconcile(t *testing.T, items []store.RemoteItem, consumable func(string, string) bool) Result {
	t.Helper()
	result, err := f.material.Reconcile(context.Background(), Input{Items: items, Consumable: consumable, SourceNames: names})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(result.Failures) > 0 {
		t.Fatalf("failures: %v", result.Failures)
	}
	return result
}

// files lists every file under the generated root, relative to it.
func (f *fixture) files(t *testing.T) []string {
	t.Helper()
	var found []string
	filepath.WalkDir(f.root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			relative, _ := filepath.Rel(f.root, path)
			found = append(found, filepath.ToSlash(relative))
		}
		return nil
	})
	sort.Strings(found)
	return found
}

func (f *fixture) read(t *testing.T, relative string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(f.root, relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(content)
}

var film = sourcecatalog.Metadata{Name: "Probe: The Film", Year: 2001, Overview: "A probe & a <film>.", ProviderIDs: map[string]string{"Tmdb": "603", "Imdb": "tt0133093"},
	Subtitles: []sourcecatalog.Subtitle{{Index: 3, Language: "eng"}, {Index: 4, Language: "eng", Forced: true}}, HasPrimaryImage: true}

// C-MA-5, C-MA-2: a movie with a strong identity from two sources is one
// folder with one source-named file each, and no path carries a provider ID.
func TestAMovieFromTwoSourcesSharesAFolder(t *testing.T) {
	f := newFixture(t)
	f.reconcile(t, []store.RemoteItem{
		item(t, "cedar", "c-1", "lib", "Movie", film),
		item(t, "walnut", "w-1", "lib", "Movie", film),
	}, everything)

	files := f.files(t)
	var strm []string
	for _, file := range files {
		if strings.HasSuffix(file, ".strm") {
			strm = append(strm, file)
		}
		if strings.Contains(file, "603") || strings.Contains(file, "tt0133093") || strings.Contains(strings.ToLower(file), "tmdb") {
			t.Fatalf("a path carries a provider identifier: %s", file)
		}
	}
	if len(strm) != 2 || filepath.Dir(strm[0]) != filepath.Dir(strm[1]) {
		t.Fatalf("both versions should share one folder: %v", strm)
	}
	folder := filepath.Base(filepath.Dir(strm[0]))
	if !strings.HasPrefix(folder, "Probe The Film (2001) [jmid-") {
		t.Fatalf("folder %q", folder)
	}
	for _, source := range []string{"Cedar", "Walnut"} {
		want := "Movies/" + folder + "/" + folder + " - " + source + ".strm"
		if !contains(files, want) {
			t.Fatalf("missing %s in %v", want, files)
		}
	}
	nfo := f.read(t, "Movies/"+folder+"/movie.nfo")
	for _, want := range []string{"<title>Probe: The Film</title>", "<tmdbid>603</tmdbid>", "<imdbid>tt0133093</imdbid>", `<uniqueid type="imdb"`, "A probe &amp; a &lt;film&gt;.", "<lockdata>true</lockdata>"} {
		if !strings.Contains(nfo, want) {
			t.Fatalf("movie.nfo lacks %q:\n%s", want, nfo)
		}
	}
	if !contains(files, "Movies/"+folder+"/poster.jpg") {
		t.Fatal("the poster should be copied")
	}
}

// C-HI-6, C-MA-5: movies without a strong identity never share a folder, even
// with the same title and year.
func TestMoviesWithoutAStrongIdentityStaySeparate(t *testing.T) {
	f := newFixture(t)
	untitled := sourcecatalog.Metadata{Name: "Home Movie", Year: 2010}
	f.reconcile(t, []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", untitled), item(t, "walnut", "w-1", "lib", "Movie", untitled)}, everything)
	folders := map[string]bool{}
	for _, file := range f.files(t) {
		if strings.HasSuffix(file, ".strm") {
			folders[filepath.Dir(file)] = true
		}
	}
	if len(folders) != 2 {
		t.Fatalf("title and year alone must not merge: %v", folders)
	}
}

// C-PR-3: a .strm holds only a local relay URL and an opaque reference; no
// generated file carries a source address, key, or token.
func TestGeneratedFilesHoldNoCredentialsOrPeerAddresses(t *testing.T) {
	f := newFixture(t)
	f.reconcile(t, []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", film)}, everything)
	records, _ := f.records.All(context.Background())
	if len(records) != 1 {
		t.Fatalf("records: %v", records)
	}
	strm := f.read(t, records[0].Path)
	if strm != relayURL+"/r/"+records[0].Reference+"\n" || len(records[0].Reference) != 32 {
		t.Fatalf(".strm content %q", strm)
	}
	for _, file := range f.files(t) {
		content := f.read(t, file)
		for _, forbidden := range []string{"cedar.example.org", "https://", "Token", "api_key", "c-1"} {
			if strings.Contains(content, forbidden) && !strings.HasSuffix(file, ".srt") && !strings.HasSuffix(file, ".jpg") {
				t.Fatalf("%s contains %q", file, forbidden)
			}
		}
	}
}

// C-MA-3: external subtitles are copied beside their reference, forced ones
// marked; a failed fetch is retried on the next pass.
func TestSubtitlesAreCopiedBesideTheReference(t *testing.T) {
	f := newFixture(t)
	f.fetcher.fail = true
	f.reconcile(t, []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", film)}, everything)
	for _, file := range f.files(t) {
		if strings.HasSuffix(file, ".srt") {
			t.Fatal("no subtitle can exist while the source is unreachable")
		}
	}
	f.fetcher.fail = false
	f.reconcile(t, []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", film)}, everything)
	var subtitles []string
	for _, file := range f.files(t) {
		if strings.HasSuffix(file, ".srt") {
			subtitles = append(subtitles, filepath.Base(file))
		}
	}
	sort.Strings(subtitles)
	if len(subtitles) != 2 || !strings.HasSuffix(subtitles[0], " - Cedar.eng.forced.srt") || !strings.HasSuffix(subtitles[1], " - Cedar.eng.srt") {
		t.Fatalf("subtitles = %v", subtitles)
	}
}

// C-MA-1: a finished file appears only by rename, metadata before the .strm,
// no temporary file is left behind, and an unchanged pass writes nothing.
func TestWritesAreAtomicAndOrdered(t *testing.T) {
	f := newFixture(t)
	var order []string
	f.material.beforeRename = func(target string) {
		if _, err := os.Stat(target); err == nil && strings.HasSuffix(target, ".strm") {
			t.Errorf("%s was visible before its rename", target)
		}
		order = append(order, filepath.Base(target))
	}
	first := f.reconcile(t, []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", film)}, everything)
	if order[len(order)-1] != filepath.Base(strings.TrimSuffix(order[len(order)-1], ".strm")+".strm") || !strings.HasSuffix(order[len(order)-1], ".strm") {
		t.Fatalf("the .strm must be placed last: %v", order)
	}
	if order[0] != "movie.nfo" {
		t.Fatalf("metadata must be placed first: %v", order)
	}
	for _, file := range f.files(t) {
		if strings.Contains(file, tempPrefix) {
			t.Fatalf("a temporary file was left behind: %s", file)
		}
	}
	if first.Written != 1 || len(first.Changed) != 1 {
		t.Fatalf("first pass: %+v", first)
	}
	order = nil
	fetches := f.fetcher.calls
	second := f.reconcile(t, []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", film)}, everything)
	if len(order) != 0 || second.Written != 0 || len(second.Changed) != 0 {
		t.Fatalf("an unchanged pass must write nothing: %v, %+v", order, second)
	}
	if f.fetcher.calls != fetches {
		t.Fatal("an unchanged pass must not fetch sidecars again")
	}
}

// C-MA-4: removal revokes the reference, deletes the item's files and only
// them, drops a folder with nothing left to play, and never touches anything
// outside the generated root.
func TestRemovalRevokesAndStaysInsideTheRoot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	both := []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", film), item(t, "walnut", "w-1", "lib", "Movie", film)}
	f.reconcile(t, both, everything)
	records, _ := f.records.All(ctx)
	var cedarReference string
	for _, record := range records {
		if record.SourceNodeID == "cedar" {
			cedarReference = record.Reference
		}
	}

	// cedar's library is opted out: its version goes, walnut's stays.
	result := f.reconcile(t, both, func(source string, _ string) bool { return source != "cedar" })
	if result.Removed != 1 || result.Written != 0 {
		t.Fatalf("removing one version must not disturb the other: %+v", result)
	}
	if _, found, _ := f.records.ByReference(ctx, cedarReference); found {
		t.Fatal("the removed version's reference must be revoked")
	}
	for _, file := range f.files(t) {
		if strings.Contains(file, " - Cedar") {
			t.Fatalf("a removed version's file remains: %s", file)
		}
	}
	// walnut's .strm and its two subtitles, and the shared movie.nfo and poster.
	if remaining := f.files(t); len(remaining) != 5 {
		t.Fatalf("walnut's version and the shared files should remain: %v", remaining)
	}

	// A record pointing outside the root is refused, not followed.
	f.records.Save(ctx, store.Materialized{SourceNodeID: "cedar", ItemID: "evil", LibraryID: "lib", Reference: "ffffffffffffffffffffffffffffffff", Path: "../precious/keep.strm"})
	refused, err := f.material.Reconcile(ctx, Input{Consumable: everything, SourceNames: names})
	if err != nil || len(refused.Failures) != 1 || !strings.Contains(refused.Failures[0], "outside the generated root") {
		t.Fatalf("a record outside the root must be refused and reported: %+v, %v", refused, err)
	}
	if _, err := os.Stat(filepath.Join(f.outside, "keep.strm")); err != nil {
		t.Fatal("a file outside the generated root was removed")
	}
	if remaining := f.files(t); len(remaining) != 0 {
		t.Fatalf("with nothing consumable, the root should be empty: %v", remaining)
	}
	if _, err := os.Stat(filepath.Join(f.root, MoviesFolder)); err != nil {
		t.Fatal("the collection folder itself stays, since Jellyfin's library points at it")
	}
}

// Episodes go in their source's series and season folders, with show and
// episode NFOs; removing the last episode removes the series folder.
func TestEpisodesAreLaidOutBySeriesAndSeason(t *testing.T) {
	f := newFixture(t)
	series := item(t, "cedar", "series-1", "lib-tv", "Series", sourcecatalog.Metadata{Name: "Probe Show", Year: 2019, ProviderIDs: map[string]string{"Tvdb": "81189"}})
	season := item(t, "cedar", "season-1", "lib-tv", "Season", sourcecatalog.Metadata{Name: "Season 1", IndexNumber: intPointer(1), SeriesID: "series-1"})
	episode := item(t, "cedar", "ep-1", "lib-tv", "Episode", sourcecatalog.Metadata{Name: "Pilot", SeriesID: "series-1", SeasonID: "season-1", ParentIndexNumber: intPointer(1), IndexNumber: intPointer(2)})
	f.reconcile(t, []store.RemoteItem{series, season, episode}, everything)
	files := f.files(t)
	var strm, show, episodeNFO string
	for _, file := range files {
		switch {
		case strings.HasSuffix(file, ".strm"):
			strm = file
		case strings.HasSuffix(file, "tvshow.nfo"):
			show = file
		case strings.HasSuffix(file, "S01E02.nfo"):
			episodeNFO = file
		}
	}
	if !strings.HasPrefix(strm, "TV Shows/Probe Show (2019) [jmid-") || !strings.HasSuffix(strm, "/Season 01/Probe Show S01E02.strm") {
		t.Fatalf("episode path %q", strm)
	}
	if !strings.Contains(f.read(t, show), "<tvdbid>81189</tvdbid>") || !strings.Contains(f.read(t, episodeNFO), "<episode>2</episode>") {
		t.Fatal("show and episode NFOs should carry identity and numbering")
	}
	f.reconcile(t, []store.RemoteItem{series, season}, everything)
	if remaining := f.files(t); len(remaining) != 0 {
		t.Fatalf("a series with no episodes left should be removed: %v", remaining)
	}
}

// An episode whose series has not arrived yet waits rather than being filed
// without one.
func TestAnEpisodeWaitsForItsSeries(t *testing.T) {
	f := newFixture(t)
	orphan := item(t, "cedar", "ep-1", "lib-tv", "Episode", sourcecatalog.Metadata{Name: "Pilot", SeriesID: "series-1"})
	if result := f.reconcile(t, []store.RemoteItem{orphan}, everything); result.Written != 0 || len(f.files(t)) != 0 {
		t.Fatal("an episode without its series must not be materialized")
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"Probe: The Film":        "Probe The Film",
		"../../etc/passwd":       "etc passwd",
		"A/B\\C":                 "A B C",
		"  ... ":                 "Untitled",
		"Tag [tmdbid-603] Title": "Tag tmdbid-603 Title",
		"Line\nbreak":            "Line break",
	}
	for input, want := range cases {
		if got := sanitize(input); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestVersionLabelsAreDistinct(t *testing.T) {
	labels := labels(map[string]string{"aaaaaaaa1": "Home", "bbbbbbbb2": "home", "cccccccc3": ""})
	if labels["aaaaaaaa1"] == labels["bbbbbbbb2"] || labels["cccccccc3"] != "Source cccccc" {
		t.Fatalf("labels = %v", labels)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestAnEmptyRootIsRefused(t *testing.T) {
	if _, err := New("  ", relayURL, nil, nil); err == nil {
		t.Fatal("an empty generated root would resolve to the working directory and must be refused")
	}
}

// M-9 found a source title that already ends with its year, which gave
// "Title (2001) (2001)".
func TestAYearIsNotRepeated(t *testing.T) {
	if name := folderName("Probe Film (2001)", 2001, "x"); strings.Count(name, "(2001)") != 1 {
		t.Fatalf("folder name %q repeats the year", name)
	}
	if name := folderName("Probe Film", 2001, "x"); !strings.HasPrefix(name, "Probe Film (2001) [jmid-") {
		t.Fatalf("folder name %q", name)
	}
}
