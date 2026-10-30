package items

import (
	"strings"
	"testing"
)

func TestSortKeysCannotInjectSQL(t *testing.T) {
	q := Query{SortBy: []string{"SortName; DROP TABLE users", "DateCreated"}, Descending: []bool{false, true}}
	sql, _ := q.PageSQL()
	if strings.Contains(sql, "DROP") {
		t.Fatalf("unknown sort key reached SQL: %s", sql)
	}
	if !strings.Contains(sql, "i.date_created DESC") {
		t.Fatalf("known sort key lost: %s", sql)
	}
}

func TestValuesAreBound(t *testing.T) {
	const attack = "x' OR '1'='1"
	q := Query{Recursive: true, Search: attack, NameStarts: attack, IncludeTypes: []string{attack}, Genres: []string{attack}}
	for _, build := range []func() (string, []any){q.PageSQL, q.CountSQL} {
		sql, args := build()
		if strings.Contains(sql, "OR '1'") {
			t.Fatalf("value reached SQL text: %s", sql)
		}
		if len(args) == 0 {
			t.Fatal("expected bound arguments")
		}
	}
}

func TestSearchEscapesWildcards(t *testing.T) {
	_, args := Query{Recursive: true, Search: "100%_"}.PageSQL()
	if got := args[len(args)-1]; got != `%100\%\_%` {
		t.Fatalf("got %v", got)
	}
}

func TestPagingAndStableOrder(t *testing.T) {
	sql, args := Query{Recursive: true, Start: 20, Limit: 10}.PageSQL()
	if !strings.HasSuffix(sql, "i.id ASC LIMIT $2 OFFSET $3") {
		t.Fatalf("got %s", sql)
	}
	if args[1] != 10 || args[2] != 20 {
		t.Fatalf("got %v", args)
	}
}

func TestTopLevelWithoutParentReturnsNothing(t *testing.T) {
	sql, _ := Query{}.CountSQL()
	if !strings.Contains(sql, "WHERE false") {
		t.Fatalf("got %s", sql)
	}
}

func TestRepeatedDirectionAppliesToRemainingKeys(t *testing.T) {
	q := Query{SortBy: []string{"ProductionYear", "SortName"}, Descending: []bool{true}}
	if order := q.order(); !strings.Contains(order, "i.sort_name DESC") {
		t.Fatalf("got %s", order)
	}
}
