package jellyfin_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"jellymesh/internal/jellyfin"
	"jellymesh/internal/jellyfin/jellyfintest"
)

const password = "service-user-password-7f3a"

func newFake(t *testing.T) *jellyfintest.Server {
	t.Helper()
	fake := jellyfintest.New()
	t.Cleanup(fake.Close)
	fake.AddLibrary("lib-movies", "Movies", "movies")
	fake.AddLibrary("lib-family", "Family Movies", "movies")
	fake.AddUser("jellymesh", password, false, "lib-movies")
	for index := 0; index < 7; index++ {
		fake.AddItem("lib-movies", jellyfin.Item{ID: fmt.Sprintf("m%d", index), Name: fmt.Sprintf("Movie %d", index), Type: "Movie", Path: fmt.Sprintf("/media/movies/Movie %d/movie.mkv", index)})
	}
	fake.AddItem("lib-family", jellyfin.Item{ID: "f1", Name: "Birthday", Type: "Movie", Path: "/media/family/birthday.mkv"})
	return fake
}

// C-SA-1: the service user sees only the libraries it has been granted.
func TestTheServiceUserSeesOnlyItsLibraries(t *testing.T) {
	fake := newFake(t)
	client := jellyfin.New(fake.URL, "jellymesh", password, "node-1")
	libraries, err := client.Libraries(context.Background())
	if err != nil {
		t.Fatalf("libraries: %v", err)
	}
	if len(libraries) != 1 || libraries[0].ID != "lib-movies" {
		t.Fatalf("libraries = %+v, want only Movies", libraries)
	}
	if page, err := client.Items(context.Background(), "lib-family", 0, 100); err != nil || len(page.Items) != 0 {
		t.Fatalf("items of an unseen library: %+v, %v", page, err)
	}
	if _, err := client.LibraryOf(context.Background(), "f1"); !errors.Is(err, jellyfin.ErrNotFound) {
		t.Fatalf("an item in an unseen library: error = %v, want ErrNotFound", err)
	}
	for _, route := range fake.Routes() {
		if strings.Contains(route, "VirtualFolders") || strings.Contains(route, "/Library/") {
			t.Fatalf("the adapter called an administrator route: %s", route)
		}
	}
}

// C-SA-1: items page, and an item's library is resolved from its ancestors.
func TestItemsPageAndResolveTheirLibrary(t *testing.T) {
	fake := newFake(t)
	client := jellyfin.New(fake.URL, "jellymesh", password, "node-1")
	seen := map[string]bool{}
	for start := 0; ; start += 3 {
		page, err := client.Items(context.Background(), "lib-movies", start, 3)
		if err != nil {
			t.Fatalf("page at %d: %v", start, err)
		}
		if page.Total != 7 {
			t.Fatalf("total = %d, want 7", page.Total)
		}
		for _, item := range page.Items {
			seen[item.ID] = true
			if item.Path == "" || item.ETag == "" {
				t.Fatalf("item %s is missing its path or ETag", item.ID)
			}
		}
		if len(page.Items) < 3 {
			break
		}
	}
	if len(seen) != 7 {
		t.Fatalf("paging returned %d distinct items, want 7", len(seen))
	}
	if library, err := client.LibraryOf(context.Background(), "m3"); err != nil || library != "lib-movies" {
		t.Fatalf("library of m3: %q, %v", library, err)
	}
}

// C-SA-1: a refused token leads to one fresh authentication, not a failure.
func TestTheAdapterReauthenticatesWhenItsTokenIsRefused(t *testing.T) {
	fake := newFake(t)
	client := jellyfin.New(fake.URL, "jellymesh", password, "node-1")
	if _, err := client.Libraries(context.Background()); err != nil {
		t.Fatalf("first: %v", err)
	}
	fake.RevokeTokens()
	if _, err := client.Libraries(context.Background()); err != nil {
		t.Fatalf("after revocation: %v", err)
	}
	authentications := 0
	for _, route := range fake.Routes() {
		if route == "POST /Users/AuthenticateByName" {
			authentications++
		}
	}
	if authentications != 2 {
		t.Fatalf("authenticated %d times, want 2", authentications)
	}
}

// A-8: an administrator account is refused as the service user.
func TestAnAdministratorIsRefusedAsTheServiceUser(t *testing.T) {
	fake := newFake(t)
	fake.AddUser("admin", password, true)
	client := jellyfin.New(fake.URL, "admin", password, "node-1")
	if _, err := client.Libraries(context.Background()); !errors.Is(err, jellyfin.ErrAuthentication) {
		t.Fatalf("error = %v, want ErrAuthentication", err)
	}
}

// C-SA-2: neither the password nor the token appears in an error.
func TestSecretsNeverAppearInErrors(t *testing.T) {
	fake := newFake(t)
	wrong := jellyfin.New(fake.URL, "jellymesh", "wrong-"+password, "node-1")
	_, err := wrong.Libraries(context.Background())
	if err == nil || strings.Contains(err.Error(), password) {
		t.Fatalf("a failed login error must not carry the password: %v", err)
	}

	client := jellyfin.New(fake.URL, "jellymesh", password, "node-1")
	client.Libraries(context.Background())
	token := client.Credentials()[1]
	if token == "" {
		t.Fatal("expected a token after a request")
	}
	fake.Close()
	_, err = client.Libraries(context.Background())
	if err == nil {
		t.Fatal("expected an error with Jellyfin down")
	}
	for _, secret := range client.Credentials() {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatalf("an error carries a secret: %v", err)
		}
	}
}
