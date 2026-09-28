package materialize

import (
	"sort"
	"strings"
)

// Works are grouped on strong identity alone (C-HI-6, design-spec section
// 10). Two items are one work when they share a provider identifier and
// disagree on none: a source that knows a film only by its IMDb identifier
// joins the source that knows it by TMDB and IMDb, but two items that share
// an IMDb identifier and carry different TMDB identifiers stay apart, and so
// does anything that joining would make inconsistent. Title and year never
// group anything.
//
// Grouping matters beyond tidiness. Jellyfin keeps a user's state for an
// item under its provider identifiers and restores it when an item with the
// same identifier appears again (conformance M-10), so a work must appear as
// one item, whose metadata carries every identifier its group knows.

// strongProviders are the identifiers that group works, in order of
// preference for naming a group.
var strongProviders = []string{"tmdb", "tvdb", "imdb"}

// member is one item to be grouped.
type member struct {
	key string            // source/item, unique
	ids map[string]string // canonical provider to value
}

// strongIDs returns an item's strong identifiers under one spelling each.
func strongIDs(providerIDs map[string]string) map[string]string {
	ids := map[string]string{}
	for key, value := range providerIDs {
		provider := strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		for _, strong := range strongProviders {
			if provider == strong {
				if provider == "imdb" {
					value = strings.ToLower(value)
				}
				ids[provider] = value
			}
		}
	}
	return ids
}

// work is a group of members and the identifiers they agree on.
type work struct {
	members []string
	ids     map[string]string
}

// identity names a work by its most preferred identifier, or by its only
// member when it has none.
func (w work) identity(kind string) string {
	for _, provider := range strongProviders {
		if value, ok := w.ids[provider]; ok {
			return kind + ":" + provider + ":" + value
		}
	}
	return kind + ":item:" + w.members[0]
}

// groupWorks partitions members into works. The result depends only on the
// members, not on their order.
func groupWorks(members []member) []work {
	sorted := append([]member(nil), members...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].key < sorted[j].key })

	parent := make([]int, len(sorted))
	ids := make([]map[string]string, len(sorted))
	for index, m := range sorted {
		parent[index] = index
		ids[index] = map[string]string{}
		for provider, value := range m.ids {
			ids[index][provider] = value
		}
	}
	var find func(int) int
	find = func(index int) int {
		if parent[index] != index {
			parent[index] = find(parent[index])
		}
		return parent[index]
	}
	compatible := func(a, b map[string]string) bool {
		for provider, value := range a {
			if other, ok := b[provider]; ok && other != value {
				return false
			}
		}
		return true
	}

	// Join along shared identifiers, most preferred provider first, each
	// value's holders in key order, so the outcome is deterministic.
	for _, provider := range strongProviders {
		holders := map[string][]int{}
		for index, m := range sorted {
			if value, ok := m.ids[provider]; ok {
				holders[value] = append(holders[value], index)
			}
		}
		values := make([]string, 0, len(holders))
		for value := range holders {
			values = append(values, value)
		}
		sort.Strings(values)
		for _, value := range values {
			indices := holders[value]
			for _, index := range indices[1:] {
				a, b := find(indices[0]), find(index)
				if a == b || !compatible(ids[a], ids[b]) {
					continue
				}
				if b < a {
					a, b = b, a
				}
				parent[b] = a
				for p, v := range ids[b] {
					ids[a][p] = v
				}
			}
		}
	}

	byRoot := map[int]*work{}
	var roots []int
	for index, m := range sorted {
		root := find(index)
		if byRoot[root] == nil {
			byRoot[root] = &work{ids: ids[root]}
			roots = append(roots, root)
		}
		byRoot[root].members = append(byRoot[root].members, m.key)
	}
	works := make([]work, 0, len(roots))
	for _, root := range roots {
		works = append(works, *byRoot[root])
	}
	return works
}
