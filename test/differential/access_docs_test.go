package differential

// JS-126: docs/access.md says what a client must send and where jellyscale is stricter than Jellyfin, and a test
// holds the second to compat/captures/access-12.4.json. The table under "## Stricter than Jellyfin" is
//
//	| Route | Jellyfin with no token | Observed |
//	| `GET /Videos/{id}/stream?static=true` | 200 | capture: `GET /Videos/{id}/stream?static=true` |
//	| any signed-in user reading every account | 200 | source only: `UserController.cs:91` and `:127` at tag v12.4 |
//
// A route is a key of the capture. The second column is the status the capture gives it with no token. A row is
// "capture: `key`" and the key is a row of the capture, or "source only:" with a file and line at tag v12.4.

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func section(md, heading string) string {
	_, rest, ok := strings.Cut(md, "\n## "+heading+"\n")
	if !ok {
		return ""
	}
	body, _, _ := strings.Cut(rest, "\n## ")
	return body
}

func TestAccessMdSaysWhatAClientMustSendNamingEachOfTheSevenSpellings(t *testing.T) {
	md, err := os.ReadFile("../../docs/access.md")
	if err != nil {
		t.Fatal(err)
	}
	body := section(string(md), "What a client must send")
	if body == "" {
		t.Fatal(`docs/access.md has no section "What a client must send"`)
	}
	for _, want := range seven {
		if !strings.Contains(body, want) {
			t.Errorf("the section does not name %q", want)
		}
	}
}

type stricterRow struct{ route, jellyfin, observed string }

func stricterRows(md string) (rows []stricterRow, found bool) {
	body := section(md, "Stricter than Jellyfin")
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "| "), "|")
		if len(cells) != 3 || strings.HasPrefix(strings.TrimSpace(cells[0]), "Route") || strings.HasPrefix(strings.TrimSpace(cells[0]), "-") {
			continue
		}
		rows = append(rows, stricterRow{strings.TrimSpace(cells[0]), strings.TrimSpace(cells[1]), strings.TrimSpace(cells[2])})
	}
	return rows, body != ""
}

var (
	captureRef = regexp.MustCompile("^capture: `([^`]+)`$")
	sourceRef  = regexp.MustCompile(`^source only: .*\.cs:\d+.*v12\.4`)
	backticked = regexp.MustCompile("`([^`]+)`")
)

// stricterProblems says what is wrong with the table: a row naming a capture row that does not exist or does not
// show what it claims, a source-only row with no file and line, a viewer route the capture shows open with no row,
// no row about reading every account.
func stricterProblems(md string, c accessCapture, routes []endpoint) []string {
	rows, ok := stricterRows(md)
	if !ok {
		return []string{`docs/access.md has no section "Stricter than Jellyfin"`}
	}
	var problems []string
	has := map[string]bool{}
	everyAccount := false
	for _, r := range rows {
		if strings.Contains(r.route, "every account") {
			everyAccount = true
		}
		m := captureRef.FindStringSubmatch(r.observed)
		if m == nil {
			if !sourceRef.MatchString(r.observed) {
				problems = append(problems, fmt.Sprintf("%q: the observed cell %q is neither capture: `key` nor source only: with a file and line at tag v12.4", r.route, r.observed))
			}
			continue
		}
		row, found := c.row(m[1])
		if !found {
			problems = append(problems, fmt.Sprintf("%q names the capture row %q, which does not exist", r.route, m[1]))
			continue
		}
		has[row.Key] = true
		if key := backticked.FindStringSubmatch(r.route); key != nil && key[1] != row.Key {
			problems = append(problems, fmt.Sprintf("the route %q names the capture row %q", key[1], row.Key))
		}
		if !strings.Contains(r.route, "every account") && r.jellyfin != strconv.Itoa(row.NoToken) {
			problems = append(problems, fmt.Sprintf("%q says Jellyfin answers %s with no token, the capture shows %d", row.Key, r.jellyfin, row.NoToken))
		}
		if !isOpen(row.NoToken) && !strings.Contains(r.route, "every account") {
			problems = append(problems, fmt.Sprintf("%q is in the table of what Jellyfin opens, and the capture shows %d", row.Key, row.NoToken))
		}
	}
	for _, row := range c.Rows {
		if is2xx(row.NoToken) && endpointAccessOf(routes, row) == "viewer" && !has[row.Key] {
			problems = append(problems, fmt.Sprintf("the capture shows Jellyfin answering %d to %q with no token, jellyscale requires a viewer, and the table has no row for it", row.NoToken, row.Key))
		}
	}
	if !everyAccount {
		problems = append(problems, `the table has no row for a signed-in user reading every account (a route cell saying "every account")`)
	}
	return problems
}

func endpointAccessOf(routes []endpoint, row accessRow) string {
	for _, e := range routes {
		if e.Path == row.Route && (e.Method == row.Method || (row.Method == "HEAD" && e.Method == "GET")) {
			return e.Access
		}
	}
	return ""
}

func TestStricterThanJellyfinHasARowForEachOpeningTheCaptureShowsAndEachRowIsBorneOut(t *testing.T) {
	md, err := os.ReadFile("../../docs/access.md")
	if err != nil {
		t.Fatal(err)
	}
	c := loadAccessCapture(t)
	for _, p := range stricterProblems(string(md), c, loadEndpoints(t)) {
		t.Error(p)
	}
}

// The check above must be able to fail: each of these tables has one thing wrong.
func TestTheStricterTableCheckCatchesARowTheCaptureDoesNotBearOut(t *testing.T) {
	c := accessCapture{Rows: []accessRow{
		{Key: "GET /Videos/{id}/stream?static=true", Method: "GET", Route: "/Videos/{itemId}/{file}", Asks: "/Videos/{id}/stream?static=true", NoToken: 200},
		{Key: "GET /Items/{id}", Method: "GET", Route: "/Items/{itemId}", Asks: "/Items/{id}", NoToken: 401},
	}}
	routes := []endpoint{{"GET", "/Videos/{itemId}/{file}", "viewer"}, {"GET", "/Items/{itemId}", "viewer"}}
	every := "| any signed-in user reading every account | 200 | source only: `UserController.cs:91` at tag v12.4 |\n"
	table := func(rows string) string {
		return "# x\n\n## Stricter than Jellyfin\n\n| Route | Jellyfin with no token | Observed |\n|---|---|---|\n" + rows + "\n## Next\n"
	}
	good := "| `GET /Videos/{id}/stream?static=true` | 200 | capture: `GET /Videos/{id}/stream?static=true` |\n" + every
	if p := stricterProblems(table(good), c, routes); len(p) != 0 {
		t.Fatalf("a right table has problems: %v", p)
	}
	for name, rows := range map[string]string{
		"a row naming no capture row":             "| `GET /Videos/{id}/stream?static=true` | 200 | capture: `GET /Videos/{id}/nothing` |\n" + every,
		"a row claiming another status":           "| `GET /Videos/{id}/stream?static=true` | 401 | capture: `GET /Videos/{id}/stream?static=true` |\n" + every,
		"a row about a route Jellyfin refuses":    good + "| `GET /Items/{id}` | 401 | capture: `GET /Items/{id}` |\n",
		"a viewer route open on Jellyfin, no row": every,
		"a source-only row with no line":          good + "| any signed-in user reading every account | 200 | source only |\n",
		"no row about every account":              "| `GET /Videos/{id}/stream?static=true` | 200 | capture: `GET /Videos/{id}/stream?static=true` |\n",
	} {
		if p := stricterProblems(table(rows), c, routes); len(p) == 0 {
			t.Errorf("%s was not caught", name)
		}
	}
}
