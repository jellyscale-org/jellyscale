package compat

// JS-126: docs/status.md has, for each of the four clients, a row of the table that starts
//
//	| Client | Direct play | HLS | Subtitles | Pictures |
//
// whose four cells say how the player's token travels for that kind, as compat/clients.yaml lists it, and how the
// list knows: "no token", "header X-Emby-Token", "query ApiKey" or "not requested", then ", from the server" where
// the URL is the server's, then "read from source" or "played on a device".

import (
	"os"
	"strings"
	"testing"

	"jellyscale/compat/conform"
)

func wantCell(p conform.PlayerRequest) []string {
	var out []string
	switch {
	case p.NotRequested:
		out = append(out, "not requested")
	case p.Token == "none":
		out = append(out, "no token")
	default:
		out = append(out, p.Token+" "+p.TokenName)
	}
	if p.URLFrom == "server" && !p.NotRequested {
		out = append(out, "from the server")
	}
	if p.Device != "" {
		out = append(out, "played on a device")
	} else {
		out = append(out, "read from source")
	}
	return out
}

func TestStatusMdSaysForEachClientHowItsPlayerSendsTheTokenAndHowThatIsKnown(t *testing.T) {
	raw, err := os.ReadFile("../docs/status.md")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "| Client | Direct play | HLS | Subtitles | Pictures |") {
			start = i
		}
	}
	if start < 0 {
		t.Fatal("docs/status.md has no table that starts | Client | Direct play | HLS | Subtitles | Pictures |")
	}
	rows := map[string][]string{}
	for _, l := range lines[start+2:] {
		if !strings.HasPrefix(l, "|") {
			break
		}
		cells := strings.Split(strings.Trim(l, "| "), "|")
		rows[strings.TrimSpace(cells[0])] = cells[1:]
	}
	kinds := []string{"direct-play", "hls", "subtitle", "picture"}
	for _, name := range conform.ClientNames {
		cells, ok := rows[name]
		if !ok || len(cells) != 4 {
			t.Errorf("the table has no row of four cells for %s: %v", name, cells)
			continue
		}
		for i, kind := range kinds {
			p := player(t, name, kind)
			for _, want := range wantCell(p) {
				if !strings.Contains(cells[i], want) {
					t.Errorf("%s %s: the cell %q does not say %q (compat/clients.yaml)", name, kind, cells[i], want)
				}
			}
			if p.Device == "" && strings.Contains(cells[i], "played on a device") {
				t.Errorf("%s %s: the cell says played on a device, and compat/clients.yaml records no device", name, kind)
			}
		}
	}
	findroid := strings.Join(rows["Findroid"], "|")
	if !strings.Contains(findroid, "401") || !strings.Contains(strings.ToLower(findroid), "direct play fails") {
		t.Errorf("Findroid's row does not say that direct play fails with 401 and why: %q", findroid)
	}
	if !strings.Contains(findroid, "no token") || !strings.Contains(findroid, "every media request") {
		t.Errorf("Findroid's row does not say its URL carries no token and jellyscale requires one on every media request: %q", findroid)
	}
}
