// Package items answers the Jellyfin item queries. Requests run in two phases:
// a compiled query picks one page of IDs, then rows hydrate from precomputed
// DTOs. Filtering and sorting stay on indexed columns; shaping stays in memory.
package items

import (
	"fmt"
	"strconv"
	"strings"
)

// Query is the subset of /Items parameters jellyscale honors.
type Query struct {
	UserID       string
	ParentID     string
	ParentIsRoot bool // ParentID names a library rather than an item.
	Recursive    bool
	IDs          []string
	IncludeTypes []string
	ExcludeTypes []string
	Genres       []string
	Years        []int
	SortBy       []string
	Descending   []bool
	Search       string
	NameStarts   string
	IsFavorite   *bool
	IsPlayed     *bool
	IsResumable  bool
	Start        int
	Limit        int
}

// sortColumns is an allowlist, because ORDER BY can't take a bound parameter.
var sortColumns = map[string]string{
	"sortname":          "i.sort_name",
	"name":              "i.sort_name",
	"datecreated":       "i.date_created",
	"premieredate":      "i.premiere_date",
	"productionyear":    "i.production_year",
	"communityrating":   "i.community_rating",
	"runtime":           "i.run_time_ticks",
	"indexnumber":       "i.index_number",
	"parentindexnumber": "i.parent_index_number",
	"dateplayed":        "d.last_played_at",
	"playcount":         "d.play_count",
	"random":            "random()",
}

type builder struct {
	where []string
	args  []any
}

func (b *builder) bind(value any) string {
	b.args = append(b.args, value)
	return "$" + strconv.Itoa(len(b.args))
}

func (b *builder) add(format string, values ...any) {
	placeholders := make([]any, len(values))
	for i, value := range values {
		placeholders[i] = b.bind(value)
	}
	b.where = append(b.where, fmt.Sprintf(format, placeholders...))
}

// compile returns the shared FROM and WHERE, and its arguments.
func (q Query) compile() (string, []any) {
	var b builder
	userID := b.bind(nullable(q.UserID))
	from := fmt.Sprintf(`FROM items i
		LEFT JOIN user_item_data d ON d.item_id = i.id AND d.user_id = %s::uuid`, userID)

	switch {
	case q.ParentID != "" && q.ParentIsRoot && q.Recursive:
		b.add("i.library_id = %s::uuid", q.ParentID)
	case q.ParentID != "" && q.ParentIsRoot:
		b.add("i.library_id = %s::uuid AND i.parent_id IS NULL", q.ParentID)
	case q.ParentID != "" && q.Recursive:
		b.add("(i.parent_id = %s::uuid OR i.series_id = %s::uuid)", q.ParentID, q.ParentID)
	case q.ParentID != "":
		b.add("i.parent_id = %s::uuid", q.ParentID)
	case !q.Recursive && len(q.IDs) == 0:
		// Top level with no parent means the views, which aren't items here.
		b.where = append(b.where, "false")
	}

	if len(q.IDs) > 0 {
		b.add("i.id = ANY(%s::uuid[])", q.IDs)
	}
	if len(q.IncludeTypes) > 0 {
		b.add("i.type = ANY(%s)", q.IncludeTypes)
	}
	if len(q.ExcludeTypes) > 0 {
		b.add("NOT i.type = ANY(%s)", q.ExcludeTypes)
	}
	if len(q.Genres) > 0 {
		b.add("i.genres && %s", q.Genres)
	}
	if len(q.Years) > 0 {
		b.add("i.production_year = ANY(%s)", q.Years)
	}
	if q.Search != "" {
		b.add("i.name ILIKE %s", "%"+escapeLike(q.Search)+"%")
	}
	if q.NameStarts != "" {
		b.add("i.sort_name LIKE %s", escapeLike(strings.ToLower(q.NameStarts))+"%")
	}
	if q.IsFavorite != nil {
		b.add("COALESCE(d.favorite, false) = %s", *q.IsFavorite)
	}
	if q.IsPlayed != nil {
		b.add("COALESCE(d.played, false) = %s", *q.IsPlayed)
	}
	if q.IsResumable {
		b.where = append(b.where, "COALESCE(d.position_ticks, 0) > 0")
	}

	if len(b.where) > 0 {
		from += "\nWHERE " + strings.Join(b.where, " AND ")
	}
	return from, b.args
}

func (q Query) order() string {
	var parts []string
	for i, key := range q.SortBy {
		column, ok := sortColumns[strings.ToLower(strings.TrimSpace(key))]
		if !ok {
			continue
		}
		// The last direction given applies to the keys after it.
		direction := "ASC"
		if last := min(i, len(q.Descending)-1); last >= 0 && q.Descending[last] {
			direction = "DESC"
		}
		parts = append(parts, column+" "+direction+" NULLS LAST")
	}
	if len(parts) == 0 {
		parts = append(parts, "i.sort_name ASC")
	}
	// A unique tiebreak keeps pages stable.
	return strings.Join(append(parts, "i.id ASC"), ", ")
}

// PageSQL selects one page of item IDs.
func (q Query) PageSQL() (string, []any) {
	from, args := q.compile()
	sql := "SELECT i.id::text " + from + "\nORDER BY " + q.order()
	if q.Limit > 0 {
		args = append(args, q.Limit)
		sql += " LIMIT $" + strconv.Itoa(len(args))
	}
	if q.Start > 0 {
		args = append(args, q.Start)
		sql += " OFFSET $" + strconv.Itoa(len(args))
	}
	return sql, args
}

// CountSQL counts every match, ignoring the page.
func (q Query) CountSQL() (string, []any) {
	from, args := q.compile()
	return "SELECT count(*) " + from, args
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}
