package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"jellymesh/internal/filmfile"
)

// SocketReader reads film bytes from the daemon's read service (package
// filmread) over a local socket. It is the mount's only connection to
// anything.
type SocketReader struct {
	client *http.Client
}

// NewSocketReader returns a reader for the socket at path.
func NewSocketReader(path string) *SocketReader {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	return &SocketReader{client: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		},
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}}}
}

// FilmPath is the read service's route for a film's bytes.
func FilmPath(reference string) string { return "/v1/films/" + reference }

func (reader *SocketReader) Read(ctx context.Context, film filmfile.Descriptor, dest []byte, offset int64) (int, error) {
	query := url.Values{
		"offset": {strconv.FormatInt(offset, 10)},
		"length": {strconv.Itoa(len(dest))},
		"size":   {strconv.FormatInt(film.Size, 10)},
	}
	if film.Bitrate > 0 {
		query.Set("bitrate", strconv.FormatInt(film.Bitrate, 10))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://jellymesh"+FilmPath(film.Reference)+"?"+query.Encode(), nil)
	if err != nil {
		return 0, err
	}
	response, err := reader.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("the read service answered %d", response.StatusCode)
	}
	count, err := io.ReadFull(response.Body, dest)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return count, err
	}
	return count, nil
}

func (reader *SocketReader) Failed(film filmfile.Descriptor) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://jellymesh"+FilmPath(film.Reference)+"/failed", nil)
	if err != nil {
		return
	}
	if response, err := reader.client.Do(request); err == nil {
		response.Body.Close()
	}
}
