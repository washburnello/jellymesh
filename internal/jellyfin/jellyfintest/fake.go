// Package jellyfintest is a fake Jellyfin for tests. It models what the
// adapter depends on and nothing more: users with per-library access,
// libraries, items with parents and paths, access tokens that can be revoked,
// and the ancestor chain of an item. It records every route called, so tests
// can assert that no administrator route is used.
package jellyfintest

import (
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
	library string
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

	default:
		http.NotFound(response, request)
	}
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
