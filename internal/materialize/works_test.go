package materialize

import (
	"reflect"
	"sort"
	"testing"
)

func partition(works []work) [][]string {
	var groups [][]string
	for _, w := range works {
		members := append([]string(nil), w.members...)
		sort.Strings(members)
		groups = append(groups, members)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	return groups
}

// C-HI-6: items sharing a strong identifier are one work, whatever the
// provider's spelling or an IMDb identifier's case, and the work carries
// every identifier its members know.
func TestWorksJoinOnASharedIdentifier(t *testing.T) {
	works := groupWorks([]member{
		{key: "cedar/1", ids: strongIDs(map[string]string{"Tmdb": "603", "Imdb": "tt0133093"})},
		{key: "walnut/9", ids: strongIDs(map[string]string{"IMDB": "TT0133093"})},
	})
	if len(works) != 1 {
		t.Fatalf("one work expected: %+v", works)
	}
	if want := map[string]string{"tmdb": "603", "imdb": "tt0133093"}; !reflect.DeepEqual(works[0].ids, want) {
		t.Fatalf("identifiers %v, want %v", works[0].ids, want)
	}
	if got := works[0].identity("movie"); got != "movie:tmdb:603" {
		t.Fatalf("identity %q", got)
	}
}

// C-HI-6: a shared identifier never joins items that disagree on another, and
// the grouping does not depend on the order items arrive in.
func TestWorksNeverJoinAcrossAConflict(t *testing.T) {
	members := []member{
		{key: "a/1", ids: map[string]string{"tmdb": "1", "imdb": "tt1"}},
		{key: "b/1", ids: map[string]string{"imdb": "tt1"}},
		{key: "c/1", ids: map[string]string{"tmdb": "2", "imdb": "tt1"}},
		{key: "d/1", ids: map[string]string{"tmdb": "2"}},
		{key: "e/1", ids: map[string]string{}},
		{key: "f/1", ids: map[string]string{}},
	}
	want := [][]string{{"a/1", "b/1"}, {"c/1", "d/1"}, {"e/1"}, {"f/1"}}
	permutations := [][]int{{0, 1, 2, 3, 4, 5}, {5, 4, 3, 2, 1, 0}, {2, 1, 0, 5, 3, 4}, {1, 3, 5, 0, 2, 4}}
	for _, order := range permutations {
		shuffled := make([]member, len(order))
		for index, from := range order {
			shuffled[index] = members[from]
		}
		works := groupWorks(shuffled)
		if got := partition(works); !reflect.DeepEqual(got, want) {
			t.Fatalf("order %v grouped %v, want %v", order, got, want)
		}
		for _, w := range works {
			if len(w.members) > 1 && w.ids["tmdb"] == "" {
				t.Fatalf("a joined work lost an identifier: %+v", w)
			}
		}
	}
}

// C-HI-6: identifiers other than the strong ones group nothing.
func TestOnlyStrongIdentifiersGroup(t *testing.T) {
	if ids := strongIDs(map[string]string{"TmdbCollection": "10", "Zap2It": "x", "Tvdb": " ", "Imdb": ""}); len(ids) != 0 {
		t.Fatalf("weak or empty identifiers were kept: %v", ids)
	}
}
