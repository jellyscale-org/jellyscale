package api

import (
	"net/http"
	"strconv"

	"jellyscale/internal/items"
)

// defaultHintLimit caps an answer to a request with no limit; what Jellyfin
// 12.4 does then is unobserved.
const defaultHintLimit = 100

// searchHints answers /Search/Hints, the suggestions Fluster shows while the
// viewer types: one hint for each item whose name holds searchTerm. Like Jellyfin
// 12.4 it cuts the matches to limit and counts the cut, and answers 400 without
// a term (compat/captures/search-hints-12.4.md).
func (s *Server) searchHints(w http.ResponseWriter, r *http.Request) {
	term := r.URL.Query().Get("searchterm")
	if term == "" {
		w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{
			"type":   "https://tools.ietf.org/html/rfc9110#section-15.5.1",
			"title":  "One or more validation errors occurred.",
			"status": http.StatusBadRequest,
			"errors": map[string]any{"searchTerm": []string{"The searchTerm field is required."}},
		})
		return
	}
	limit := defaultHintLimit
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, _ = strconv.Atoi(value)
	}
	hints := []map[string]any{}
	if limit > 0 {
		q := items.Query{
			UserID:       sessionOf(r).User.ID,
			Recursive:    true,
			IncludeTypes: queryList(r, "includeitemtypes"),
			Search:       term,
			HintOrder:    true,
			Limit:        limit,
		}
		result, err := s.Items.Find(r.Context(), q, items.ParseFields(""), false)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, item := range result.Items {
			hints = append(hints, searchHint(item))
		}
	}
	writeJSON(w, map[string]any{"SearchHints": hints, "TotalRecordCount": len(hints)})
}

// searchHint is the SearchHint of an item's DTO: the keys Jellyfin gives a hint,
// where the item has them.
func searchHint(item map[string]any) map[string]any {
	hint := map[string]any{"ItemId": item["Id"], "Artists": []string{}, "ChannelId": nil}
	for _, key := range []string{"Id", "Name", "Type", "MediaType", "RunTimeTicks", "ProductionYear",
		"IndexNumber", "ParentIndexNumber", "PrimaryImageAspectRatio"} {
		if value, ok := item[key]; ok {
			hint[key] = value
		}
	}
	if item["IsFolder"] == true {
		hint["IsFolder"] = true
	}
	if name, ok := item["SeriesName"]; ok && item["Type"] == "Episode" {
		hint["Series"] = name
	}
	if tags, ok := item["ImageTags"].(map[string]any); ok && tags["Primary"] != "" && tags["Primary"] != nil {
		hint["PrimaryImageTag"] = tags["Primary"]
	}
	if hint["MediaType"] == nil {
		hint["MediaType"] = "Unknown" // a series, as in Jellyfin
	}
	return hint
}
