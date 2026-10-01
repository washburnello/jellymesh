// Package jellyfintest is a fake Jellyfin for tests. It models what the
// adapter depends on and nothing more: users with per-library access,
// libraries, items with parents and paths, access tokens that can be revoked,
// and the ancestor chain of an item. It records every route called, so tests
// can assert that no administrator route is used.
package jellyfintest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"jellymesh/internal/jellyfin"
)

type user struct {
	id        string
	password  string
	admin     bool
	libraries map[string]bool
}

type item struct {
	jellyfin.Item
	library   string
	media     []byte
	subtitles map[int]string
	image     []byte
}

// Server is a running fake Jellyfin.
type Server struct {
	*httptest.Server

	mutex     sync.Mutex
	users     map[string]*user
	tokens    map[string]string
	libraries map[string]jellyfin.Library
	items     map[string]*item
	routes    []string
	notices   int
}

// New starts a fake Jellyfin.
func New() *Server {
	fake := &Server{
		users: map[string]*user{}, tokens: map[string]string{},
		libraries: map[string]jellyfin.Library{}, items: map[string]*item{},
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(fake.serve))
	return fake
}

// AddUser creates a user that can see the given libraries.
func (fake *Server) AddUser(name string, password string, admin bool, libraries ...string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	visible := map[string]bool{}
	for _, library := range libraries {
		visible[library] = true
	}
	fake.users[name] = &user{id: randomID(), password: password, admin: admin, libraries: visible}
}

// Grant lets a user see a library.
func (fake *Server) Grant(name string, library string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.users[name].libraries[library] = true
}

// AddLibrary creates a library.
func (fake *Server) AddLibrary(id string, name string, collectionType string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.libraries[id] = jellyfin.Library{ID: id, Name: name, CollectionType: collectionType}
}

// AddItem adds an item to a library. ETag defaults to "1".
func (fake *Server) AddItem(library string, value jellyfin.Item) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if value.ETag == "" {
		value.ETag = "1"
	}
	fake.items[value.ID] = &item{Item: value, library: library}
}

// Update changes an item's name and ETag, as an edit in Jellyfin would.
func (fake *Server) Update(id string, name string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	current := fake.items[id]
	current.Name = name
	etag, _ := strconv.Atoi(current.ETag)
	current.ETag = strconv.Itoa(etag + 1)
}

// SetPath changes an item's path.
func (fake *Server) SetPath(id string, path string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.items[id].Path = path
}

// Move puts an item in another library.
func (fake *Server) Move(id string, library string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.items[id].library = library
}

// Delete removes an item.
func (fake *Server) Delete(id string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	delete(fake.items, id)
}

// SetMedia gives an item playable content, served by the stream route.
func (fake *Server) SetMedia(id string, content []byte) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	current := fake.items[id]
	current.media = content
	if len(current.MediaSources) == 0 {
		current.MediaSources = []jellyfin.MediaSource{{ID: "ms-" + id}}
	}
	current.MediaSources[0].Size = int64(len(content))
	current.MediaSources[0].Container = "mkv"
}

// AddSubtitle gives an item an external subtitle at a stream index.
func (fake *Server) AddSubtitle(id string, index int, language string, text string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	current := fake.items[id]
	if current.subtitles == nil {
		current.subtitles = map[int]string{}
	}
	current.subtitles[index] = text
	if len(current.MediaSources) == 0 {
		current.MediaSources = []jellyfin.MediaSource{{ID: "ms-" + id}}
	}
	current.MediaSources[0].MediaStreams = append(current.MediaSources[0].MediaStreams,
		jellyfin.MediaStream{Index: index, Type: "Subtitle", Codec: "subrip", Language: language, IsExternal: true})
}

// SetImage gives an item a primary image.
func (fake *Server) SetImage(id string, content []byte) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	current := fake.items[id]
	current.image = content
	current.ImageTags = map[string]string{"Primary": "tag-" + id}
}

// RevokeTokens invalidates every issued access token.
func (fake *Server) RevokeTokens() {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.tokens = map[string]string{}
}

// Routes returns every route called so far, as "METHOD /path".
func (fake *Server) Routes() []string {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return append([]string(nil), fake.routes...)
}

var tokenPattern = regexp.MustCompile(`Token="([^"]*)"`)

func (fake *Server) serve(response http.ResponseWriter, request *http.Request) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.routes = append(fake.routes, request.Method+" "+request.URL.Path)

	if request.Method == http.MethodPost && request.URL.Path == "/Users/AuthenticateByName" {
		var body struct{ Username, Pw string }
		json.NewDecoder(request.Body).Decode(&body)
		account, ok := fake.users[body.Username]
		if !ok || account.password != body.Pw {
			http.Error(response, "denied", http.StatusUnauthorized)
			return
		}
		token := randomID()
		fake.tokens[token] = body.Username
		writeJSON(response, map[string]any{
			"AccessToken": token,
			"User":        map[string]any{"Id": account.id, "Policy": map[string]bool{"IsAdministrator": account.admin}},
		})
		return
	}

	match := tokenPattern.FindStringSubmatch(request.Header.Get("Authorization"))
	name, ok := "", false
	if match != nil {
		name, ok = fake.tokens[match[1]]
	}
	if !ok {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	account := fake.users[name]

	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/UserViews":
		var visible []jellyfin.Library
		for id, library := range fake.libraries {
			if account.admin || account.libraries[id] {
				visible = append(visible, library)
			}
		}
		sort.Slice(visible, func(i, j int) bool { return visible[i].ID < visible[j].ID })
		writeJSON(response, map[string]any{"Items": visible, "TotalRecordCount": len(visible)})

	case request.Method == http.MethodGet && request.URL.Path == "/Items":
		query := request.URL.Query()
		library := query.Get("ParentId")
		if !account.admin && !account.libraries[library] {
			writeJSON(response, jellyfin.Page{Items: []jellyfin.Item{}})
			return
		}
		var matching []jellyfin.Item
		for _, candidate := range fake.items {
			if candidate.library == library {
				matching = append(matching, candidate.Item)
			}
		}
		sort.Slice(matching, func(i, j int) bool { return matching[i].Name+matching[i].ID < matching[j].Name+matching[j].ID })
		start, _ := strconv.Atoi(query.Get("StartIndex"))
		limit, _ := strconv.Atoi(query.Get("Limit"))
		total := len(matching)
		if start > total {
			start = total
		}
		end := total
		if limit > 0 && start+limit < total {
			end = start + limit
		}
		writeJSON(response, jellyfin.Page{Items: matching[start:end], Total: total})

	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/Items/") && strings.HasSuffix(request.URL.Path, "/Ancestors"):
		id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/Items/"), "/Ancestors")
		found, ok := fake.items[id]
		if !ok || (!account.admin && !account.libraries[found.library]) {
			http.NotFound(response, request)
			return
		}
		ancestors := []map[string]string{}
		if found.ParentID != "" {
			ancestors = append(ancestors, map[string]string{"Id": found.ParentID, "Type": "Folder"})
		}
		ancestors = append(ancestors,
			map[string]string{"Id": found.library, "Type": "CollectionFolder"},
			map[string]string{"Id": "root", "Type": "UserRootFolder"})
		writeJSON(response, ancestors)

	case strings.HasPrefix(request.URL.Path, "/Videos/") && strings.HasSuffix(request.URL.Path, "/stream"):
		// Like real Jellyfin (phase-0-results.md section 8), the stream route
		// does not check the user's library access at all.
		id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/Videos/"), "/stream")
		found, ok := fake.items[id]
		if !ok || found.media == nil {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "video/x-matroska")
		http.ServeContent(response, request, "", time.Time{}, bytes.NewReader(found.media))

	case strings.HasPrefix(request.URL.Path, "/Videos/") && strings.Contains(request.URL.Path, "/Subtitles/"):
		parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/Videos/"), "/")
		if len(parts) < 4 {
			http.NotFound(response, request)
			return
		}
		found, ok := fake.items[parts[0]]
		index, _ := strconv.Atoi(parts[3])
		if !ok || found.subtitles[index] == "" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/x-subrip")
		response.Write([]byte(found.subtitles[index]))

	case strings.HasPrefix(request.URL.Path, "/Items/") && strings.HasSuffix(request.URL.Path, "/Images/Primary"):
		id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/Items/"), "/Images/Primary")
		found, ok := fake.items[id]
		if !ok || found.image == nil {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "image/jpeg")
		response.Write(found.image)

	case request.Method == http.MethodPost && request.URL.Path == "/Library/Media/Updated":
		fake.notices++
		response.WriteHeader(http.StatusNoContent)

	default:
		http.NotFound(response, request)
	}
}

// Notices reports how many change notices were received.
func (fake *Server) Notices() int {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.notices
}

func writeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	json.NewEncoder(response).Encode(value)
}

func randomID() string {
	random := make([]byte, 16)
	rand.Read(random)
	return hex.EncodeToString(random)
}
