package catalogsync

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellymesh/internal/jellyfin"
	"jellymesh/internal/replication"
	"jellymesh/internal/sourcecatalog"
	"jellymesh/internal/store"
)

// withMedia is newWorld with media, a subtitle, and an image on movie-1, and
// the source serving them through counting, which records bytes read from
// Jellyfin.
func withMedia(t *testing.T, ceiling int64) (*world, []byte, *counting) {
	t.Helper()
	w := newWorld(t)
	content := bytes.Repeat([]byte("jellymesh"), 1<<17) // about 1.1 MB
	w.fake.SetMedia("movie-1", content)
	w.fake.AddSubtitle("movie-1", 3, "eng", "1\n00:00:01,000 --> 00:00:02,000\nHello\n")
	w.fake.SetImage("movie-1", []byte("poster-bytes"))
	if err := w.catalog.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	counter := &counting{MediaSource: jellyfin.New(w.fake.URL, "jellymesh", "service-pw", "cedar")}
	w.server.SetMedia(counter, ceiling)
	return w, content, counter
}

// counting wraps a media source and counts the bytes read from its streams.
type counting struct {
	MediaSource
	read atomic.Int64
}

func (source *counting) Stream(ctx context.Context, method string, item string, rangeHeader string) (*http.Response, error) {
	response, err := source.MediaSource.Stream(ctx, method, item, rangeHeader)
	if err == nil {
		response.Body = &countingBody{ReadCloser: response.Body, count: &source.read}
	}
	return response, err
}

type countingBody struct {
	io.ReadCloser
	count *atomic.Int64
}

func (body *countingBody) Read(buffer []byte) (int, error) {
	read, err := body.ReadCloser.Read(buffer)
	body.count.Add(int64(read))
	return read, err
}

func (w *world) media(t *testing.T, method string, path string, rangeHeader string) (*http.Response, []byte) {
	t.Helper()
	header := http.Header{}
	if rangeHeader != "" {
		header.Set("Range", rangeHeader)
	}
	client := replication.NewClient(w.walnut.identity, w.walnut.group)
	response, err := client.Stream(context.Background(), w.source, method, path, header)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response, body
}

// C-PB-1: a member streams a published item with byte ranges and HEAD, the
// response carrying length and type, and exactly the bytes asked for.
func TestAMemberStreamsWithRangesAndHead(t *testing.T) {
	w, content, _ := withMedia(t, 0)
	response, body := w.media(t, http.MethodGet, "/jellymesh/v1/groups/group-1/media/movie-1", "bytes=100-4195")
	if response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[100:4196]) {
		t.Fatalf("range: status %d, %d bytes", response.StatusCode, len(body))
	}
	if response.Header.Get("Content-Range") == "" || response.Header.Get("Content-Type") != "video/x-matroska" {
		t.Fatalf("headers: %v", response.Header)
	}
	head, headBody := w.media(t, http.MethodHead, "/jellymesh/v1/groups/group-1/media/movie-1", "")
	if head.StatusCode != http.StatusOK || len(headBody) != 0 || head.ContentLength != int64(len(content)) {
		t.Fatalf("head: status %d, length %d, body %d", head.StatusCode, head.ContentLength, len(headBody))
	}
	whole, wholeBody := w.media(t, http.MethodGet, "/jellymesh/v1/groups/group-1/media/movie-1", "")
	if whole.StatusCode != http.StatusOK || !bytes.Equal(wholeBody, content) {
		t.Fatal("a whole-file request should return the file exactly")
	}
}

// C-CA-4 at the media route: Jellyfin's stream route ignores library
// permissions, so the source's own live check is what refuses a moved,
// unpublished, or unknown item.
func TestTheMediaRouteAuthorizesAgainstLiveState(t *testing.T) {
	w, _, _ := withMedia(t, 0)
	w.fake.Move("movie-1", "lib-docs-private")
	if response, _ := w.media(t, http.MethodGet, "/jellymesh/v1/groups/group-1/media/movie-1", ""); response.StatusCode != http.StatusNotFound {
		t.Fatalf("an item moved out of its published library: status %d, want 404", response.StatusCode)
	}
	if response, _ := w.media(t, http.MethodGet, "/jellymesh/v1/groups/group-1/media/not-in-catalog", ""); response.StatusCode != http.StatusNotFound {
		t.Fatalf("an item not in the catalog: status %d, want 404", response.StatusCode)
	}
	w.catalog.Unpublish(context.Background(), "lib-docs")
	if response, _ := w.media(t, http.MethodGet, "/jellymesh/v1/groups/group-1/media/doc-1", ""); response.StatusCode != http.StatusNotFound {
		t.Fatalf("an unpublished item: status %d, want 404", response.StatusCode)
	}
}

// C-MA-3: a listed external subtitle and the primary image can be fetched;
// an index the catalog does not list cannot.
func TestSubtitlesAndImagesAreServedOnlyAsListed(t *testing.T) {
	w, _, _ := withMedia(t, 0)
	page, _ := w.catalog.Changes(context.Background(), 0, 1000, nil)
	var metadata *sourcecatalog.Metadata
	for _, change := range page.Changes {
		if change.ItemID == "movie-1" {
			metadata = change.Metadata
		}
	}
	if metadata == nil || len(metadata.Subtitles) != 1 || metadata.Subtitles[0].Language != "eng" || !metadata.HasPrimaryImage {
		t.Fatalf("catalog metadata should list the subtitle and image: %+v", metadata)
	}
	if response, body := w.media(t, http.MethodGet, "/jellymesh/v1/groups/group-1/subtitles/movie-1/3", ""); response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("Hello")) {
		t.Fatalf("subtitle: status %d, %q", response.StatusCode, body)
	}
	// A subtitle Jellyfin has but the catalog does not list, as after an edit
	// the catalog has not yet seen, is refused rather than passed through.
	w.fake.AddSubtitle("movie-1", 4, "fre", "1\n00:00:01,000 --> 00:00:02,000\nBonjour\n")
	if response, _ := w.media(t, http.MethodGet, "/jellymesh/v1/groups/group-1/subtitles/movie-1/4", ""); response.StatusCode != http.StatusNotFound {
		t.Fatalf("an unlisted subtitle index: status %d, want 404", response.StatusCode)
	}
	if response, body := w.media(t, http.MethodGet, "/jellymesh/v1/groups/group-1/images/movie-1/primary", ""); response.StatusCode != http.StatusOK || string(body) != "poster-bytes" {
		t.Fatalf("image: status %d", response.StatusCode)
	}
}

// C-PB-1: when the destination stops reading, the source stops reading from
// Jellyfin rather than pulling the rest of the file.
func TestADisconnectStopsTheReadFromJellyfin(t *testing.T) {
	w, content, counter := withMedia(t, 0)
	large := bytes.Repeat([]byte("x"), 32<<20)
	w.fake.SetMedia("movie-1", large)
	_ = content
	ctx, cancel := context.WithCancel(context.Background())
	client := replication.NewClient(w.walnut.identity, w.walnut.group)
	response, err := client.Stream(ctx, w.source, http.MethodGet, "/jellymesh/v1/groups/group-1/media/movie-1", nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	first := make([]byte, 64<<10)
	io.ReadFull(response.Body, first)
	cancel()
	response.Body.Close()
	time.Sleep(300 * time.Millisecond)
	if read := counter.read.Load(); read >= int64(len(large))/2 {
		t.Fatalf("the source read %d of %d bytes after the destination left", read, len(large))
	}
}

// C-PB-3: one destination's streams together stay under its ceiling.
func TestTheCeilingBoundsADestinationsStreamsTogether(t *testing.T) {
	const ceiling = 1 << 20 // 1 MiB per second
	w, _, _ := withMedia(t, ceiling)
	w.fake.SetMedia("movie-1", bytes.Repeat([]byte("y"), 768<<10))
	start := time.Now()
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			w.media(t, http.MethodGet, "/jellymesh/v1/groups/group-1/media/movie-1", "")
		}()
	}
	group.Wait()
	// 1.5 MiB at 1 MiB/s, less the permitted burst of a quarter second.
	if elapsed := time.Since(start); elapsed < 1200*time.Millisecond {
		t.Fatalf("two streams of 768 KiB finished in %v; the ceiling allows no less than about 1.25 s", elapsed)
	}
}

var _ = store.SourceItem{}
