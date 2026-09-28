// Package jellyfin reads a node's own Jellyfin server as a dedicated
// non-administrator service user (conformance.md assumption A-8).
//
// It uses only routes an ordinary user may call. It never holds an
// administrator key, so a library the service user cannot see is invisible
// here and can never be published, and compromising Jellymesh does not grant
// Jellyfin administration.
//
// The password and access token travel only in request bodies and headers.
// No error this package returns contains either, and callers should still
// register both with the audit redactor.
package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ItemTypes are the item kinds Jellymesh federates. Music is out of scope for
// this release (conformance.md assumption A-3).
var ItemTypes = []string{"Movie", "Series", "Season", "Episode"}

var (
	ErrAuthentication = errors.New("jellyfin refused the service user's credentials")
	ErrNotFound       = errors.New("jellyfin has no such item visible to the service user")
)

// Library is a library the service user can see.
type Library struct {
	ID             string `json:"Id"`
	Name           string `json:"Name"`
	CollectionType string `json:"CollectionType"`
}

// Item is the subset of Jellyfin's item record that Jellymesh uses.
type Item struct {
	ID                string            `json:"Id"`
	Name              string            `json:"Name"`
	OriginalTitle     string            `json:"OriginalTitle,omitempty"`
	Type              string            `json:"Type"`
	ParentID          string            `json:"ParentId,omitempty"`
	SeriesID          string            `json:"SeriesId,omitempty"`
	SeasonID          string            `json:"SeasonId,omitempty"`
	IndexNumber       *int              `json:"IndexNumber,omitempty"`
	ParentIndexNumber *int              `json:"ParentIndexNumber,omitempty"`
	ProductionYear    int               `json:"ProductionYear,omitempty"`
	PremiereDate      string            `json:"PremiereDate,omitempty"`
	Overview          string            `json:"Overview,omitempty"`
	OfficialRating    string            `json:"OfficialRating,omitempty"`
	Genres            []string          `json:"Genres,omitempty"`
	RunTimeTicks      int64             `json:"RunTimeTicks,omitempty"`
	ProviderIDs       map[string]string `json:"ProviderIds,omitempty"`
	// Path and ETag stay on the source. The path reveals filesystem layout
	// and is used only to check the item against the declared roots.
	Path string `json:"Path,omitempty"`
	ETag string `json:"Etag,omitempty"`

	ImageTags    map[string]string `json:"ImageTags,omitempty"`
	MediaSources []MediaSource     `json:"MediaSources,omitempty"`
}

// MediaSource is one playable source of an item.
type MediaSource struct {
	ID           string        `json:"Id"`
	MediaStreams []MediaStream `json:"MediaStreams,omitempty"`
}

// MediaStream is one stream of a media source. Only external subtitles
// matter to Jellymesh: embedded streams travel inside the media itself.
type MediaStream struct {
	Index      int    `json:"Index"`
	Type       string `json:"Type"`
	Codec      string `json:"Codec,omitempty"`
	Language   string `json:"Language,omitempty"`
	IsExternal bool   `json:"IsExternal,omitempty"`
	IsForced   bool   `json:"IsForced,omitempty"`
	IsDefault  bool   `json:"IsDefault,omitempty"`
}

// Page is one page of a library's items.
type Page struct {
	Items []Item `json:"Items"`
	Total int    `json:"TotalRecordCount"`
}

// Client is a Jellyfin client for one service user.
type Client struct {
	base     string
	username string
	password string
	deviceID string
	http     *http.Client
	// streaming has no overall timeout, since a stream lasts as long as
	// playback does; the caller's context bounds it instead.
	streaming *http.Client

	mutex  sync.Mutex
	token  string
	userID string
}

// New returns a client for the Jellyfin at baseURL. deviceID identifies this
// node to Jellyfin, which ties its session to one device.
func New(baseURL string, username string, password string, deviceID string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"), username: username, password: password, deviceID: deviceID,
		http:      &http.Client{Timeout: 60 * time.Second},
		streaming: &http.Client{},
	}
}

// Credentials returns the secrets this client holds, for registration with an
// audit redactor. The token is empty until the first request.
func (client *Client) Credentials() []string {
	client.mutex.Lock()
	defer client.mutex.Unlock()
	return []string{client.password, client.token}
}

func (client *Client) authorization(token string) string {
	header := fmt.Sprintf(`MediaBrowser Client="Jellymesh", Device="Jellymesh", DeviceId=%q, Version="1"`, client.deviceID)
	if token != "" {
		header += fmt.Sprintf(`, Token=%q`, token)
	}
	return header
}

func (client *Client) authenticate(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"Username": client.username, "Pw": client.password})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.base+"/Users/AuthenticateByName", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", client.authorization(""))
	response, err := client.http.Do(request)
	if err != nil {
		return fmt.Errorf("reach jellyfin: %w", scrub(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w (%s)", ErrAuthentication, response.Status)
	}
	var result struct {
		AccessToken string `json:"AccessToken"`
		User        struct {
			ID     string `json:"Id"`
			Policy struct {
				IsAdministrator bool `json:"IsAdministrator"`
			} `json:"Policy"`
		} `json:"User"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("decode authentication: %w", err)
	}
	if result.AccessToken == "" || result.User.ID == "" {
		return fmt.Errorf("%w: no token issued", ErrAuthentication)
	}
	if result.User.Policy.IsAdministrator {
		// The whole point of the service user is that it is not one.
		return fmt.Errorf("%w: the service user %q is a Jellyfin administrator; use a dedicated non-administrator user", ErrAuthentication, client.username)
	}
	client.token, client.userID = result.AccessToken, result.User.ID
	return nil
}

// get performs an authenticated GET, authenticating first if needed and once
// more if Jellyfin refuses the token.
func (client *Client) get(ctx context.Context, path string, query url.Values, into any) error {
	for attempt := 0; attempt < 2; attempt++ {
		client.mutex.Lock()
		if client.token == "" {
			if err := client.authenticate(ctx); err != nil {
				client.mutex.Unlock()
				return err
			}
		}
		token, userID := client.token, client.userID
		client.mutex.Unlock()

		if query == nil {
			query = url.Values{}
		}
		query.Set("userId", userID)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.base+path+"?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", client.authorization(token))
		response, err := client.http.Do(request)
		if err != nil {
			return fmt.Errorf("reach jellyfin: %w", scrub(err))
		}
		switch response.StatusCode {
		case http.StatusOK:
			err := json.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(into)
			response.Body.Close()
			if err != nil {
				return fmt.Errorf("decode %s: %w", path, err)
			}
			return nil
		case http.StatusUnauthorized:
			response.Body.Close()
			client.mutex.Lock()
			if client.token == token {
				client.token = ""
			}
			client.mutex.Unlock()
			continue
		case http.StatusNotFound:
			response.Body.Close()
			return ErrNotFound
		default:
			response.Body.Close()
			return fmt.Errorf("jellyfin answered %s for %s", response.Status, path)
		}
	}
	return ErrAuthentication
}

// scrub strips the URL from a transport error. The URL carries no secret,
// but a transport error's text is not something this package controls.
func scrub(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}

// Libraries returns the libraries the service user can see.
func (client *Client) Libraries(ctx context.Context) ([]Library, error) {
	var result struct {
		Items []Library `json:"Items"`
	}
	if err := client.get(ctx, "/UserViews", nil, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// Items returns one page of a library's federated items, parents before
// children within the page's type ordering.
func (client *Client) Items(ctx context.Context, libraryID string, startIndex int, limit int) (Page, error) {
	query := url.Values{
		"ParentId":         {libraryID},
		"Recursive":        {"true"},
		"IncludeItemTypes": {strings.Join(ItemTypes, ",")},
		"Fields":           {"ProviderIds,Path,Etag,Overview,Genres,OriginalTitle,PremiereDate,OfficialRating,MediaSources,MediaStreams"},
		"SortBy":           {"SortName"},
		"StartIndex":       {strconv.Itoa(startIndex)},
		"Limit":            {strconv.Itoa(limit)},
	}
	var page Page
	err := client.get(ctx, "/Items", query, &page)
	return page, err
}

// LibraryOf returns the library an item belongs to now, as Jellyfin reports
// it, by walking its ancestors to the collection folder. It is what a
// per-item request is authorized against, because a catalog built earlier
// may be stale about where the item lives.
func (client *Client) LibraryOf(ctx context.Context, itemID string) (string, error) {
	var ancestors []struct {
		ID   string `json:"Id"`
		Type string `json:"Type"`
	}
	if err := client.get(ctx, "/Items/"+url.PathEscape(itemID)+"/Ancestors", nil, &ancestors); err != nil {
		return "", err
	}
	for _, ancestor := range ancestors {
		if ancestor.Type == "CollectionFolder" {
			return ancestor.ID, nil
		}
	}
	return "", ErrNotFound
}

// Stream opens an item's original media as the service user, forwarding the
// caller's Range header. It uses the static stream route, which serves the
// file as stored, honours ranges and HEAD, and is open to a non-administrator
// (conformance M-8). The caller closes the response body.
func (client *Client) Stream(ctx context.Context, method string, itemID string, rangeHeader string) (*http.Response, error) {
	query := url.Values{"static": {"true"}}
	return client.raw(ctx, method, "/Videos/"+url.PathEscape(itemID)+"/stream", query, map[string]string{"Range": rangeHeader})
}

// Subtitle opens one external subtitle of an item as SubRip text.
func (client *Client) Subtitle(ctx context.Context, itemID string, mediaSourceID string, index int) (*http.Response, error) {
	path := fmt.Sprintf("/Videos/%s/%s/Subtitles/%d/0/Stream.srt", url.PathEscape(itemID), url.PathEscape(mediaSourceID), index)
	return client.raw(ctx, http.MethodGet, path, nil, nil)
}

// PrimaryImage opens an item's primary image.
func (client *Client) PrimaryImage(ctx context.Context, itemID string) (*http.Response, error) {
	return client.raw(ctx, http.MethodGet, "/Items/"+url.PathEscape(itemID)+"/Images/Primary", nil, nil)
}

// NotifyUpdated tells Jellyfin that paths changed. A non-administrator's
// notice is accepted but may have no effect (conformance M-8), so this is
// best effort and its failure is not an error for the caller to act on.
func (client *Client) NotifyUpdated(ctx context.Context, paths []string, updateType string) error {
	type update struct {
		Path       string `json:"Path"`
		UpdateType string `json:"UpdateType"`
	}
	body := struct {
		Updates []update `json:"Updates"`
	}{}
	for _, path := range paths {
		body.Updates = append(body.Updates, update{Path: path, UpdateType: updateType})
	}
	encoded, _ := json.Marshal(body)
	response, err := client.rawBody(ctx, http.MethodPost, "/Library/Media/Updated", nil, nil, encoded)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode >= 300 {
		return fmt.Errorf("jellyfin answered %s to a change notice", response.Status)
	}
	return nil
}

// raw performs an authenticated request and returns the response for the
// caller to stream, authenticating again once if the token is refused. Any
// status other than 401 is returned to the caller.
func (client *Client) raw(ctx context.Context, method string, path string, query url.Values, headers map[string]string) (*http.Response, error) {
	return client.rawBody(ctx, method, path, query, headers, nil)
}

func (client *Client) rawBody(ctx context.Context, method string, path string, query url.Values, headers map[string]string, body []byte) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		client.mutex.Lock()
		if client.token == "" {
			if err := client.authenticate(ctx); err != nil {
				client.mutex.Unlock()
				return nil, err
			}
		}
		token := client.token
		client.mutex.Unlock()

		target := client.base + path
		if len(query) > 0 {
			target += "?" + query.Encode()
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		request, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", client.authorization(token))
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		for name, value := range headers {
			if value != "" {
				request.Header.Set(name, value)
			}
		}
		response, err := client.streaming.Do(request)
		if err != nil {
			return nil, fmt.Errorf("reach jellyfin: %w", scrub(err))
		}
		if response.StatusCode == http.StatusUnauthorized {
			response.Body.Close()
			client.mutex.Lock()
			if client.token == token {
				client.token = ""
			}
			client.mutex.Unlock()
			continue
		}
		return response, nil
	}
	return nil, ErrAuthentication
}
