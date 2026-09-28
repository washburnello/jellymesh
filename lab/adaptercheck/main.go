// Command adaptercheck runs Jellymesh's Jellyfin adapter against a real
// Jellyfin and reports whether each route behaves as internal/jellyfin
// assumes (conformance.md M-7). It reads the service user's credentials from
// the environment and prints no secret.
//
//	JF_URL=http://127.0.0.1:18098 JF_USER=jellymesh JF_PASSWORD=... go run ./lab/adaptercheck
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"jellymesh/internal/jellyfin"
)

func main() {
	url, user, password := os.Getenv("JF_URL"), os.Getenv("JF_USER"), os.Getenv("JF_PASSWORD")
	if url == "" || user == "" || password == "" {
		fmt.Fprintln(os.Stderr, "set JF_URL, JF_USER, and JF_PASSWORD")
		os.Exit(2)
	}
	ctx := context.Background()
	client := jellyfin.New(url, user, password, "jellymesh-adaptercheck")
	failures := 0
	check := func(name string, ok bool, detail string) {
		mark := "PASS"
		if !ok {
			mark, failures = "FAIL", failures+1
		}
		fmt.Printf("%s  %-44s %s\n", mark, name, detail)
	}

	libraries, err := client.Libraries(ctx)
	check("authenticate as a non-administrator", err == nil, errText(err))
	check("/UserViews lists the granted libraries", err == nil && len(libraries) > 0, fmt.Sprintf("%d visible", len(libraries)))

	var sample jellyfin.Item
	for _, library := range libraries {
		page, err := client.Items(ctx, library.ID, 0, 100)
		detail := fmt.Sprintf("%s (%s): %d of %d", library.Name, library.CollectionType, len(page.Items), page.Total)
		check("/Items pages a library", err == nil, detail+errText(err))
		for _, item := range page.Items {
			check("  item "+item.Type+" carries Path and Etag", item.Path != "" || item.Type == "Season", pathShape(item))
			if item.ETag == "" {
				check("  item "+item.Type+" carries an Etag", false, item.ID)
			}
			if sample.ID == "" && (item.Type == "Episode" || item.Type == "Movie") {
				sample = item
				owner, err := client.LibraryOf(ctx, item.ID)
				check("/Items/{id}/Ancestors resolves the library", err == nil && owner == library.ID, fmt.Sprintf("%s -> %s%s", item.Type, owner, errText(err)))
			}
			if item.Type == "Episode" {
				check("  episode names its series and season", item.SeriesID != "" && item.SeasonID != "", "")
			}
			if item.Type == "Movie" {
				// run-throwaway.sh gives the film an NFO with these.
				check("  movie carries studios, taglines, and ratings",
					len(item.Studios) == 1 && item.Studios[0].Name == "Probe Pictures" && len(item.Taglines) == 1 && item.CommunityRating == 7.5 && item.CriticRating == 81,
					fmt.Sprintf("%v %q %v %v", item.Studios, item.Taglines, item.CommunityRating, item.CriticRating))
			}
		}
		if page.Total > 1 {
			second, err := client.Items(ctx, library.ID, 1, 1)
			check("/Items honours StartIndex and Limit", err == nil && len(second.Items) == 1, "")
		}
	}

	// The service user must not reach an administrator route.
	token := client.Credentials()[1]
	status := call(url, "GET", "/Library/VirtualFolders", token)
	check("an administrator route is refused", status == http.StatusForbidden || status == http.StatusUnauthorized, fmt.Sprintf("HTTP %d", status))

	// Revoke the token and confirm the adapter authenticates again.
	logout := call(url, "POST", "/Sessions/Logout", token)
	_, err = client.Libraries(ctx)
	check("a revoked token leads to re-authentication", err == nil && client.Credentials()[1] != token, fmt.Sprintf("logout HTTP %d%s", logout, errText(err)))

	if failures > 0 {
		fmt.Printf("\n%d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}

func call(base string, method string, path string, token string) int {
	request, _ := http.NewRequest(method, base+path, nil)
	request.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Client="Jellymesh", Device="Jellymesh", DeviceId="jellymesh-adaptercheck", Version="1", Token=%q`, token))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0
	}
	response.Body.Close()
	return response.StatusCode
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return " error: " + err.Error()
}

// pathShape shows the path's directory depth without printing it in full.
func pathShape(item jellyfin.Item) string {
	if item.Path == "" {
		return "(no path)"
	}
	return fmt.Sprintf("%d path components", strings.Count(item.Path, "/"))
}
