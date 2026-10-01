package materialize

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"jellymesh/internal/filmfile"
	"jellymesh/internal/sourcecatalog"
	"jellymesh/internal/store"
)

// sizingFetcher also answers a film's size, as the daemon does with a HEAD to
// the source.
type sizingFetcher struct {
	fetcher
	sizes map[string]int64
}

func (f *sizingFetcher) Size(_ context.Context, _ string, item string) (int64, error) {
	if size, ok := f.sizes[item]; ok {
		return size, nil
	}
	return 0, errors.New("no such item")
}

func filesFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.material.SetPresentation(PresentFiles)
	return f
}

func withFile(metadata sourcecatalog.Metadata, size int64, extension string) sourcecatalog.Metadata {
	metadata.File = &sourcecatalog.File{Size: size, Bitrate: 2_000_000, Extension: extension}
	return metadata
}

func (f *fixture) descriptor(t *testing.T, relative string) filmfile.Descriptor {
	t.Helper()
	film, err := filmfile.Decode([]byte(f.read(t, relative)))
	if err != nil {
		t.Fatalf("%s is not a descriptor: %v", relative, err)
	}
	return film
}

// C-FS-1: in file presentation each film is a descriptor in the layout .strm
// presentation uses, named for the film the mount shows, with sidecars that
// match that name. It holds the reference, the size, and nothing else of
// note: no address, key, or token.
func TestFilesPresentationWritesDescriptorsInTheSameLayout(t *testing.T) {
	f := filesFixture(t)
	f.reconcile(t, []store.RemoteItem{
		item(t, "cedar", "c-1", "lib", "Movie", withFile(film, 1469241147, "mkv")),
		item(t, "walnut", "w-1", "lib", "Movie", withFile(film, 700, "MP4")),
	}, everything)

	files := f.files(t)
	var descriptors []string
	for _, file := range files {
		if strings.HasSuffix(file, ".strm") {
			t.Fatalf("file presentation wrote a .strm: %s", file)
		}
		if strings.HasSuffix(file, filmfile.Suffix) {
			descriptors = append(descriptors, file)
		}
	}
	if len(descriptors) != 2 {
		t.Fatalf("one descriptor per version: %v", files)
	}
	records, _ := f.records.All(context.Background())
	for _, record := range records {
		folder := record.Path[:strings.LastIndex(record.Path, "/")]
		name := folder[strings.LastIndex(folder, "/")+1:]
		label, extension, size := "Cedar", "mkv", int64(1469241147)
		if record.SourceNodeID == "walnut" {
			label, extension, size = "Walnut", "mp4", 700
		}
		stem := folder + "/" + name + " - " + label
		if record.Path != stem+"."+extension+filmfile.Suffix {
			t.Fatalf("descriptor path %q", record.Path)
		}
		descriptor := f.descriptor(t, record.Path)
		if descriptor.Reference != record.Reference || descriptor.Size != size || descriptor.Bitrate != 2_000_000 || descriptor.Modified == 0 {
			t.Fatalf("descriptor %+v for %+v", descriptor, record)
		}
		for _, sidecar := range []string{stem + ".eng.srt", stem + ".eng.forced.srt", folder + "/movie.nfo", folder + "/poster.jpg"} {
			if !contains(files, sidecar) {
				t.Fatalf("missing %s in %v", sidecar, files)
			}
		}
		content := f.read(t, record.Path)
		if strings.Contains(content, "http") || strings.Contains(content, "127.0.0.1") {
			t.Fatalf("a descriptor names an address: %s", content)
		}
	}
}

// C-FS-1: a descriptor's modification time stays put while the film is
// unchanged, since Jellyfin probes a film again whenever it moves, and moves
// when the film changes.
func TestADescriptorKeepsItsTimeUntilTheFilmChanges(t *testing.T) {
	f := filesFixture(t)
	now := time.Unix(1_800_000_000, 0)
	f.material.now = func() time.Time { return now }
	items := []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", withFile(film, 1000, "mkv"))}
	f.reconcile(t, items, everything)
	records, _ := f.records.All(context.Background())
	first := f.descriptor(t, records[0].Path)

	now = now.Add(48 * time.Hour)
	if result := f.reconcile(t, items, everything); result.Written != 0 {
		t.Fatalf("an unchanged film must not be rewritten: %+v", result)
	}
	if again := f.descriptor(t, records[0].Path); again.Modified != first.Modified {
		t.Fatalf("the time moved for an unchanged film: %d, then %d", first.Modified, again.Modified)
	}

	replaced := []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", withFile(film, 2000, "mkv"))}
	replaced[0].Revision = 2
	f.reconcile(t, replaced, everything)
	changed := f.descriptor(t, records[0].Path)
	if changed.Size != 2000 || changed.Modified != now.Unix() || changed.Reference != first.Reference {
		t.Fatalf("a replaced file should keep its reference and take the new size and time: %+v", changed)
	}
}

// C-FS-5: marking a film changed moves its time forward an hour, keeping
// everything else, and a later pass keeps the moved time.
func TestMarkChangedMovesTheTimeAnHour(t *testing.T) {
	f := filesFixture(t)
	items := []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", withFile(film, 1000, "mkv"))}
	f.reconcile(t, items, everything)
	records, _ := f.records.All(context.Background())
	before := f.descriptor(t, records[0].Path)
	if err := f.material.MarkChanged(context.Background(), records[0].Reference); err != nil {
		t.Fatalf("mark changed: %v", err)
	}
	after := f.descriptor(t, records[0].Path)
	if after.Modified != before.Modified+3600 || after.Reference != before.Reference || after.Size != before.Size {
		t.Fatalf("before %+v, after %+v", before, after)
	}
	f.reconcile(t, items, everything)
	if kept := f.descriptor(t, records[0].Path); kept.Modified != after.Modified {
		t.Fatalf("a pass undid the change: %d", kept.Modified)
	}
	if err := f.material.MarkChanged(context.Background(), "00000000000000000000000000000000"); err != nil {
		t.Fatalf("an unknown reference is not an error: %v", err)
	}
}

// C-FS-1: a film whose catalog lacks its size is sized at the source, and
// one that cannot be sized is reported and not shown, never shown with a
// wrong size.
func TestAFilmWithoutASizeIsSizedAtItsSource(t *testing.T) {
	f := filesFixture(t)
	sizing := &sizingFetcher{sizes: map[string]int64{"c-1": 4242}}
	f.material.fetch = sizing
	result, err := f.material.Reconcile(context.Background(), Input{Items: []store.RemoteItem{
		item(t, "cedar", "c-1", "lib", "Movie", film),
		item(t, "cedar", "c-2", "lib", "Movie", sourcecatalog.Metadata{Name: "Unsized", Year: 1999}),
	}, Consumable: everything, SourceNames: names})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(result.Failures) != 1 || !strings.Contains(result.Failures[0], ErrNoSize.Error()) {
		t.Fatalf("the unsized film should be reported: %+v", result)
	}
	records, _ := f.records.All(context.Background())
	if len(records) != 1 || f.descriptor(t, records[0].Path).Size != 4242 {
		t.Fatalf("only the sized film should be shown: %+v", records)
	}
	for _, file := range f.files(t) {
		if strings.Contains(file, "Unsized") && strings.HasSuffix(file, filmfile.Suffix) {
			t.Fatalf("an unsized film was shown: %s", file)
		}
	}
}

// C-FS-1: removal in file presentation removes the descriptor and its
// sidecars, revokes the reference, and prunes the folder.
func TestRemovingAFilmRemovesItsDescriptor(t *testing.T) {
	f := filesFixture(t)
	items := []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", withFile(film, 1000, "mkv"))}
	f.reconcile(t, items, everything)
	records, _ := f.records.All(context.Background())
	f.reconcile(t, nil, everything)
	if remaining := f.files(t); len(remaining) != 0 {
		t.Fatalf("files remain: %v", remaining)
	}
	if _, found, _ := f.records.ByReference(context.Background(), records[0].Reference); found {
		t.Fatal("the reference must be revoked")
	}
}

// C-FS-6: switching presentation moves every item to its new form at the
// next pass, removing the old one, and an episode keeps its numbering.
func TestSwitchingPresentationMovesEveryItem(t *testing.T) {
	f := newFixture(t)
	series := item(t, "cedar", "series-1", "lib-tv", "Series", sourcecatalog.Metadata{Name: "Probe Show", Year: 2019, ProviderIDs: map[string]string{"Tvdb": "81189"}})
	season := item(t, "cedar", "season-1", "lib-tv", "Season", sourcecatalog.Metadata{Name: "Season 1", IndexNumber: intPointer(1), SeriesID: "series-1"})
	episode := item(t, "cedar", "ep-1", "lib-tv", "Episode", withFile(sourcecatalog.Metadata{Name: "Pilot", SeriesID: "series-1", SeasonID: "season-1", ParentIndexNumber: intPointer(1), IndexNumber: intPointer(2)}, 10, "mkv"))
	items := []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", withFile(film, 1000, "mkv")), series, season, episode}
	f.reconcile(t, items, everything)

	f.material.SetPresentation(PresentFiles)
	result := f.reconcile(t, items, everything)
	if result.Removed != 2 || result.Written != 2 {
		t.Fatalf("both items should move: %+v", result)
	}
	files := f.files(t)
	var descriptors int
	episodeShown := false
	for _, file := range files {
		episodeShown = episodeShown || strings.HasSuffix(file, "/Season 01/Probe Show S01E02.mkv"+filmfile.Suffix)
		if strings.HasSuffix(file, ".strm") {
			t.Fatalf("a .strm remains: %s", file)
		}
		if strings.HasSuffix(file, filmfile.Suffix) {
			descriptors++
		}
	}
	if descriptors != 2 || !episodeShown {
		t.Fatalf("after the switch: %v", files)
	}

	f.material.SetPresentation(PresentStrm)
	f.reconcile(t, items, everything)
	for _, file := range f.files(t) {
		if strings.HasSuffix(file, filmfile.Suffix) {
			t.Fatalf("switching back left a descriptor: %s", file)
		}
	}
}

// C-FS-1: without a size and without a way to find one, a film is reported
// and not shown.
func TestAFilmWithNoWayToSizeItIsNotShown(t *testing.T) {
	f := filesFixture(t)
	result, err := f.material.Reconcile(context.Background(), Input{Items: []store.RemoteItem{item(t, "cedar", "c-1", "lib", "Movie", film)},
		Consumable: everything, SourceNames: names})
	if err != nil || len(result.Failures) != 1 || !strings.Contains(result.Failures[0], ErrNoSize.Error()) {
		t.Fatalf("an unsized film should be reported: %+v, %v", result, err)
	}
	for _, file := range f.files(t) {
		if strings.HasSuffix(file, filmfile.Suffix) {
			t.Fatalf("an unsized film was shown: %s", file)
		}
	}
}
