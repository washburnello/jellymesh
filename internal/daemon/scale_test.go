package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellymesh/internal/jellyfin"
	"jellymesh/internal/jellyfin/jellyfintest"
)

func intPointer(value int) *int { return &value }

// TestFirstJoinAtCedarScale measures a first join against a source the size
// of cedar's publishable libraries (M-5): 255 films and 35 series with 150
// seasons and 2,994 episodes. It is a measurement, not a gate, so it runs
// only with JELLYMESH_SCALE=1.
func TestFirstJoinAtCedarScale(t *testing.T) {
	if os.Getenv("JELLYMESH_SCALE") == "" {
		t.Skip("set JELLYMESH_SCALE=1 to measure")
	}
	source, destination := jellyfintest.New(), jellyfintest.New()
	defer source.Close()
	defer destination.Close()
	source.AddLibrary("lib-movies", "Movies", "movies")
	source.AddLibrary("lib-shows", "Shows", "tvshows")
	source.AddUser("jellymesh", "service-user-password-for-tests", false, "lib-movies", "lib-shows")
	for i := 0; i < 255; i++ {
		source.AddItem("lib-movies", jellyfin.Item{ID: fmt.Sprintf("m%d", i), Name: fmt.Sprintf("Film %d", i), ProductionYear: 1980 + i%40,
			Type: "Movie", Path: fmt.Sprintf("/media/movies/f%d.mkv", i), ProviderIDs: map[string]string{"Tmdb": fmt.Sprint(10000 + i)}})
	}
	episodes := 0
	for s := 0; s < 35; s++ {
		series := fmt.Sprintf("s%d", s)
		source.AddItem("lib-shows", jellyfin.Item{ID: series, Name: fmt.Sprintf("Show %d", s), Type: "Series",
			Path: "/media/tv/" + series, ProviderIDs: map[string]string{"Tvdb": fmt.Sprint(70000 + s)}})
		seasons := 4 + s%3 // 150 seasons across 35 series, give or take
		for n := 1; n <= seasons && episodes < 2994; n++ {
			season := fmt.Sprintf("%s-%d", series, n)
			source.AddItem("lib-shows", jellyfin.Item{ID: season, Name: fmt.Sprintf("Season %d", n), Type: "Season", SeriesID: series, IndexNumber: intPointer(n)})
			for e := 1; e <= 20 && episodes < 2994; e++ {
				source.AddItem("lib-shows", jellyfin.Item{ID: fmt.Sprintf("%s-%d", season, e), Name: fmt.Sprintf("Episode %d", e), Type: "Episode",
					SeriesID: series, SeasonID: season, ParentIndexNumber: intPointer(n), IndexNumber: intPointer(e),
					Path: fmt.Sprintf("/media/tv/%s/%d/%d.mkv", series, n, e)})
				episodes++
			}
		}
	}
	destination.AddLibrary("lib-docs", "Docs", "movies")
	destination.AddUser("jellymesh", "service-user-password-for-tests", false, "lib-docs")
	destination.AddItem("lib-docs", jellyfin.Item{ID: "d1", Name: "Doc", Type: "Movie", Path: "/media/docs/d.mkv"})

	cedar := startDaemonWith(t, "cedar", t.TempDir(), "127.0.0.1:0", withServiceUser(source))
	walnutDir := t.TempDir()
	walnut := startDaemonWith(t, "walnut", walnutDir, "127.0.0.1:0", withServiceUser(destination))
	cedar.must(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-1"}, nil)
	began := time.Now()
	cedar.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-movies", Roots: []string{"/media/movies"}}, nil)
	cedar.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-shows", Roots: []string{"/media/tv"}}, nil)
	walnut.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-docs", Roots: []string{"/media/docs"}}, nil)
	cedar.catalogSync()
	published := time.Since(began)
	joinOffering(t, cedar, walnut, cedar, nil)
	began = time.Now()
	result := walnut.catalogSync()
	synced := time.Since(began)
	files, strm := 0, 0
	filepath.WalkDir(filepath.Join(walnutDir, "generated"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			files++
			if strings.HasSuffix(path, ".strm") {
				strm++
			}
		}
		return nil
	})
	t.Logf("source catalog built in %v; destination synced and materialized in %v: %d written, %d files, %d .strm (%d films + %d episodes expected)",
		published.Round(time.Millisecond), synced.Round(time.Millisecond), result.Materialized.Written, files, strm, 255, episodes)
	if strm != 255+episodes {
		t.Fatalf("materialized %d playable files, want %d", strm, 255+episodes)
	}
}
