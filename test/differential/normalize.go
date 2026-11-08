// Package differential compares what a real Jellyfin 12.4 and jellyscale answer
// to the same requests. This file makes two answers comparable (Normalize) and
// finds where they still differ (Diff); allowlist.go says which differences are
// known.
package differential

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Difference is one place where the two answers disagree. Jellyfin or
// Jellyscale is nil where that side has no value at Path.
type Difference struct {
	Path                 string
	Jellyfin, Jellyscale any
}

// A 32 digit id, with or without dashes, which is how the two servers write one.
var idPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}`)

// MaskedTimeKeys are the keys whose timestamp changes from run to run (when
// the fixtures were scanned, played or signed in). Every other timestamp, such
// as PremiereDate, is stable and compared. Documented in docs/testing.md.
var MaskedTimeKeys = map[string]bool{
	"DateCreated": true, "DateLastMediaAdded": true, "LastPlayedDate": true,
	"LastActivityDate": true, "LastLoginDate": true,
}

// Tokens and session ids in a query string.
var queryPattern = regexp.MustCompile(`(?i)\b(api_key|ApiKey|PlaySessionId|DeviceId)=[^&"\s]*`)

// Fields whose value is made per server or per run. The key stays, so that a
// field one server leaves out is still a difference.
var maskedKeys = map[string]bool{
	"AccessToken": true, "Etag": true, "ETag": true, "PlaySessionId": true,
	"ServerId": true, "SessionId": true, "LiveStreamId": true,
}

func canonicalID(id string) string {
	return strings.ToLower(strings.ReplaceAll(id, "-", ""))
}

// ItemKeys maps the id of every object in doc that has a string Id, Name and
// Type to "Type/Name", which names the same item on either server.
func ItemKeys(doc any) map[string]string {
	keys := map[string]string{}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			id, _ := v["Id"].(string)
			name, _ := v["Name"].(string)
			typ, _ := v["Type"].(string)
			if id != "" && name != "" && typ != "" {
				keys[id] = typ + "/" + name
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	return keys
}

// Normalize returns a copy of doc that does not depend on which server made it:
//   - an id in ids becomes "<id:KEY>", also inside longer strings such as URLs;
//   - the timestamps under MaskedTimeKeys, and the value of tokens, ETags and session and server ids, are masked;
//   - unless ordered, every array is sorted, because most listings promise no order.
//
// An id that is not in ids stays as it is and shows up as a difference.
func Normalize(doc any, ids map[string]string, ordered bool) any {
	byCanonical := make(map[string]string, len(ids))
	for id, key := range ids {
		byCanonical[canonicalID(id)] = key
	}
	n := normalizer{ids: byCanonical, ordered: ordered}
	return n.value(doc)
}

type normalizer struct {
	ids     map[string]string
	ordered bool
}

// realTime says whether a masked time key holds a real timestamp. An empty,
// zero or unparsable value is kept, so it still differs from Jellyfin's.
func realTime(text string) bool {
	t, err := time.Parse(time.RFC3339Nano, text)
	return err == nil && t.Year() > 1
}

func (n normalizer) value(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			if _, isString := child.(string); isString && maskedKeys[k] {
				out[k] = "<masked>"
				continue
			}
			if text, isString := child.(string); isString && MaskedTimeKeys[k] && realTime(text) {
				out[k] = "<time>"
				continue
			}
			out[k] = n.value(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = n.value(child)
		}
		if !n.ordered {
			encoded := make([]string, len(out))
			for i, child := range out {
				b, _ := json.Marshal(child)
				encoded[i] = string(b)
			}
			sort.Sort(byEncoding{out, encoded})
		}
		return out
	case string:
		s := idPattern.ReplaceAllStringFunc(v, func(id string) string {
			if key, ok := n.ids[canonicalID(id)]; ok {
				return "<id:" + key + ">"
			}
			return id
		})
		return queryPattern.ReplaceAllString(s, "${1}=<masked>")
	}
	return v
}

type byEncoding struct {
	items   []any
	encoded []string
}

func (b byEncoding) Len() int           { return len(b.items) }
func (b byEncoding) Less(i, j int) bool { return b.encoded[i] < b.encoded[j] }
func (b byEncoding) Swap(i, j int) {
	b.items[i], b.items[j] = b.items[j], b.items[i]
	b.encoded[i], b.encoded[j] = b.encoded[j], b.encoded[i]
}

// Diff lists where two decoded JSON documents differ, with paths like
// "Items[2].UserData.Played".
func Diff(jellyfin, jellyscale any) []Difference {
	var out []Difference
	diff("", jellyfin, jellyscale, &out)
	return out
}

func diff(path string, a, b any, out *[]Difference) {
	switch a := a.(type) {
	case map[string]any:
		if b, ok := b.(map[string]any); ok {
			keys := make([]string, 0, len(a)+len(b))
			for k := range a {
				keys = append(keys, k)
			}
			for k := range b {
				if _, both := a[k]; !both {
					keys = append(keys, k)
				}
			}
			slices.Sort(keys)
			for _, k := range keys {
				child := k
				if path != "" {
					child = path + "." + k
				}
				av, aok := a[k]
				bv, bok := b[k]
				switch {
				case !bok:
					*out = append(*out, Difference{Path: child, Jellyfin: av})
				case !aok:
					*out = append(*out, Difference{Path: child, Jellyscale: bv})
				default:
					diff(child, av, bv, out)
				}
			}
			return
		}
	case []any:
		if b, ok := b.([]any); ok {
			for i := 0; i < max(len(a), len(b)); i++ {
				child := fmt.Sprintf("%s[%d]", path, i)
				switch {
				case i >= len(b):
					*out = append(*out, Difference{Path: child, Jellyfin: a[i]})
				case i >= len(a):
					*out = append(*out, Difference{Path: child, Jellyscale: b[i]})
				default:
					diff(child, a[i], b[i], out)
				}
			}
			return
		}
	default:
		if a == b {
			return
		}
	}
	// Different types, or different scalars.
	*out = append(*out, Difference{Path: path, Jellyfin: a, Jellyscale: b})
}

// IDDifferences compares which items each array of items holds, by their
// normalized Id, and in what order when ordered. An allowlist entry that hides
// a field cannot hide a dropped, extra or reordered item, because those show
// here at the path "<array> (ids)" ("(ids)" for the top level). Both documents
// must be normalized.
func IDDifferences(jellyfin, jellyscale any, ordered bool) []Difference {
	a, b := idLists(jellyfin, ordered), idLists(jellyscale, ordered)
	var out []Difference
	for _, path := range sortedKeys(a, b) {
		x, y := a[path], b[path]
		if slices.Equal(x, y) {
			continue
		}
		d := Difference{Path: strings.TrimSpace(path + " (ids)")}
		if x != nil {
			d.Jellyfin = x
		}
		if y != nil {
			d.Jellyscale = y
		}
		out = append(out, d)
	}
	return out
}

// idLists maps the path of each array whose elements have an Id to those Ids.
func idLists(doc any, ordered bool) map[string][]string {
	lists := map[string][]string{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, child := range v {
				walk(strings.TrimPrefix(path+"."+k, "."), child)
			}
		case []any:
			var ids []string
			for i, child := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
				if m, ok := child.(map[string]any); ok {
					if id, ok := m["Id"].(string); ok {
						ids = append(ids, id)
					}
				}
			}
			if ids != nil {
				if !ordered {
					slices.Sort(ids)
				}
				lists[path] = ids
			}
		}
	}
	walk("", doc)
	return lists
}

func sortedKeys(a, b map[string][]string) []string {
	var keys []string
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, both := a[k]; !both {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}
