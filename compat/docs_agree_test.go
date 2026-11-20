package compat

// JS-139: the documents agree with the tasks marked done in tasks/done.json, with each other and with their own JSON.
// Each test reads documents and asserts what they must say; none depends on the code. A statement that stops being
// true must change in every place that makes it, so a place is named by the words that open it (a heading, a bullet,
// a paragraph) and the test fails when one is gone, so a rename cannot hide a place from it.

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func docText(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../" + rel)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// lineOf is the one line of text that starts with prefix.
func lineOf(t *testing.T, text, prefix string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one line starting %q, found %d", prefix, len(found))
	}
	return found[0]
}

// linesContaining is every line of text that matches re.
func linesContaining(text string, re *regexp.Regexp) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if re.MatchString(l) {
			out = append(out, l)
		}
	}
	return out
}

// taskStatus is the status of every task in tasks/prd.json and tasks/done.json.
func taskStatus(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range []string{"tasks/prd.json", "tasks/done.json"} {
		var doc struct {
			UserStories []struct{ ID, Status string } `json:"userStories"`
		}
		if err := json.Unmarshal([]byte(docText(t, f)), &doc); err != nil {
			t.Fatal(err)
		}
		for _, s := range doc.UserStories {
			out[s.ID] = s.Status
		}
	}
	return out
}

func decisionIDs(t *testing.T) map[string]bool {
	t.Helper()
	var doc struct {
		Decisions []struct{ ID string } `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(docText(t, "tasks/decisions.json")), &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, d := range doc.Decisions {
		out[d.ID] = true
	}
	return out
}

var taskRef = regexp.MustCompile(`JS-\d+`)

func mustMatch(t *testing.T, where, text, what, pattern string) {
	t.Helper()
	if !regexp.MustCompile(pattern).MatchString(text) {
		t.Errorf("%s does not say %s (pattern %s)", where, what, pattern)
	}
}

func mustNotMatch(t *testing.T, where, text, what, pattern string) {
	t.Helper()
	if m := regexp.MustCompile(pattern).FindString(text); m != "" {
		t.Errorf("%s still says %s: %q", where, what, m)
	}
}

// --- docs/status.md against the tasks marked done ---

func TestStatusDoesNotCallWhatIsBuiltNotBuilt(t *testing.T) {
	status := docText(t, "docs/status.md")
	for what, pattern := range map[string]string{
		"the api replica half of the gate is not built (JS-031)":         `(?i)(api|API) replica half is not built|(api|API) replica killed mid-film: \*\*not built\*\*|not yet shown to lose nothing`,
		"the AV1 fallback is not built (JS-133)":                         `(?i)AV1 fallback is not built`,
		"encoding to AV1 is not built, for the copy's fallback (JS-133)": `Not built:[^.]*encoding to AV1`,
		"HDR direct play and copy are not built (JS-025)":                `(?i)Not built:[^.]*HDR direct play`,
		"Dolby Vision profile 5 is not mapped, as a fact":                `profile 5, which has no HDR10 base, is not mapped`,
	} {
		mustNotMatch(t, "docs/status.md", status, what, pattern)
	}
}

func TestStatusNamesTheTaskThatBuiltWhatItNowSays(t *testing.T) {
	status := docText(t, "docs/status.md")
	gate := lineOf(t, status, "Gate for Phase 4 (HA)")
	ha := lineOf(t, status, "- A worker killed mid-film recovers")
	hevc := lineOf(t, status, "- HEVC and AV1 to clients that play them")
	tone := lineOf(t, status, "- HDR tone mapping to SDR")
	segments := lineOf(t, status, "- A film recovers from a failed segment")
	for _, c := range []struct{ where, line, task string }{
		{"the gate paragraph (api and stream replicas)", gate, "JS-031"},
		{"the gate paragraph (AV1 fallback)", gate, "JS-133"},
		{"the phase4-ha line", ha, "JS-031"},
		{"the HEVC and AV1 line (encoding to AV1 for the fallback)", hevc, "JS-133"},
		{"the tone mapping line (HDR copies)", tone, "JS-025"},
		{"the JS-088 line", segments, "JS-133"},
	} {
		if !strings.Contains(c.line, c.task) {
			t.Errorf("%s does not name %s", c.where, c.task)
		}
	}
	// "the stream replica half" and the api half are both said built where the gate is.
	mustMatch(t, "the gate paragraph", gate, "that the api and stream replica half is built", `(?is)(api|API)[^.]{0,200}replica[^.]{0,300}JS-031|JS-031[^.]{0,300}(api|API)`)
	mustMatch(t, "the tone mapping line", tone, "that HDR direct play and copy are built (VIDEO-RANGE=PQ or HLG)", `VIDEO-RANGE=PQ`)
	mustMatch(t, "the HEVC and AV1 line", hevc, "that an AV1 fallback encode is built", `(?i)SVT-AV1|AV1 (fallback|encode)`)

	// Every task a corrected line names is done, never dropped and never open.
	st := taskStatus(t)
	for where, line := range map[string]string{"gate": gate, "phase4-ha": ha, "hevc": hevc, "tone": tone, "segments": segments} {
		for _, id := range taskRef.FindAllString(line, -1) {
			// The tone mapping line also names JS-149, the open task that closes the profile 5 gap (D-021).
			if id == "JS-149" {
				if st[id] != "todo" && st[id] != "in_progress" {
					t.Errorf("status.md's %s line names %s as the closer of a gap, but its status is %q, not open", where, id, st[id])
				}
				continue
			}
			if st[id] != "done" {
				t.Errorf("status.md's %s line names %s, whose status is %q, not done", where, id, st[id])
			}
		}
	}
}

func TestStatusNamesNoTaskThatDoesNotExist(t *testing.T) {
	st := taskStatus(t)
	for _, id := range taskRef.FindAllString(docText(t, "docs/status.md"), -1) {
		if _, ok := st[id]; !ok {
			t.Errorf("docs/status.md names %s, which is in neither tasks/prd.json nor tasks/done.json", id)
		}
	}
}

func TestStatusSaysTheSameOfTheReplicaHalfAndTheAV1FallbackEverywhere(t *testing.T) {
	for _, path := range []string{"docs/status.md", "docs/transcoding.md", "DOMAINS.md"} {
		text := docText(t, path)
		mustNotMatch(t, path, text, "the api replica half of the gate is not built", `(?i)(api|recovering the api) replica (half|killed mid-film)[^.]{0,200}(not built|not yet shown|is Phase 4 and not built)|Not built: an `+"`api`"+` replica killed mid-film`)
		mustNotMatch(t, path, text, "the AV1 fallback is not built", `(?i)(the )?AV1 fallback is not built|none for AV1`)
	}
}

// --- the known gaps, together ---

// gapSection is the bullets under the heading of status.md that lists its known gaps.
func gapSection(t *testing.T) []string {
	t.Helper()
	var bullets []string
	in, level := false, 0
	for _, l := range strings.Split(docText(t, "docs/status.md"), "\n") {
		if m := regexp.MustCompile(`^(#+) (.*)$`).FindStringSubmatch(l); m != nil {
			if in && len(m[1]) <= level {
				break
			}
			if !in && regexp.MustCompile(`(?i)gaps?\b`).MatchString(m[2]) {
				in, level = true, len(m[1])
			}
			continue
		}
		if in && strings.HasPrefix(l, "- ") {
			bullets = append(bullets, l)
		}
	}
	if len(bullets) == 0 {
		t.Fatal("docs/status.md has no heading about known gaps with bullets under it")
	}
	return bullets
}

func gapWith(t *testing.T, bullets []string, what, pattern string) string {
	t.Helper()
	var hit []string
	for _, b := range bullets {
		if regexp.MustCompile(pattern).MatchString(b) {
			hit = append(hit, b)
		}
	}
	if len(hit) != 1 {
		t.Errorf("the known gaps have %d bullets for %s (pattern %s), want exactly 1", len(hit), what, pattern)
		return ""
	}
	return hit[0]
}

// closedWithNoTask is true when the gap says "no task" and not "no task yet" (RE2 has no lookahead).
func closedWithNoTask(b string) bool {
	return regexp.MustCompile(`(?i)\bno task\b`).MatchString(b) && !regexp.MustCompile(`(?i)\bno task yet\b`).MatchString(b)
}

func TestEveryKnownGapIsCalledAGapAndHasOneCloser(t *testing.T) {
	st, decisions := taskStatus(t), decisionIDs(t)
	for _, b := range gapSection(t) {
		short := b
		if len(short) > 60 {
			short = short[:60]
		}
		if !regexp.MustCompile(`(?i)\bgap\b`).MatchString(b) {
			t.Errorf("%q is listed under the known gaps but never calls itself a gap", short)
		}
		tasks, ds := taskRef.FindAllString(b, -1), regexp.MustCompile(`D-\d{3}`).FindAllString(b, -1)
		for _, id := range tasks {
			if st[id] != "todo" && st[id] != "in_progress" {
				t.Errorf("%q names %s, whose status is %q: a gap's task is open in tasks/prd.json", short, id, st[id])
			}
		}
		for _, d := range ds {
			if !decisions[d] {
				t.Errorf("%q names %s, which is not in tasks/decisions.json", short, d)
			}
		}
		noTaskYet := regexp.MustCompile(`(?i)\bno task yet\b`).MatchString(b)
		noTask := regexp.MustCompile(`(?i)\bno task\b`).MatchString(b) && !noTaskYet
		switch {
		case noTaskYet && (len(ds) == 0 || len(tasks) == 0):
			t.Errorf("%q says 'no task yet' without the decision id and the open task that will decide it", short)
		case noTask && len(ds) == 0:
			t.Errorf("%q says 'no task' without the decision id that accepted it", short)
		case !noTask && !noTaskYet && len(tasks) == 0:
			t.Errorf("%q names no open task, 'no task' or 'no task yet'", short)
		}
		if strings.Count(strings.ToLower(b), "no task") > 1 {
			t.Errorf("%q has more than one closer", short)
		}
	}
}

func TestTheKnownGapsListWhatTheDocumentsDisagreedAbout(t *testing.T) {
	g := gapSection(t)

	cap := gapWith(t, g, "the 1080 line ceiling of a re-encode", `1080 lines`)
	for what, pattern := range map[string]string{
		"H.264":                              `H\.264`,
		"SDR":                                `SDR`,
		"a 4K film":                          `4K`,
		"an HDR film":                        `HDR`,
		"a burned-in image subtitle":         `(?i)image subtitle`,
		"no task yet, D-021 and JS-148":      `(?is)no task yet.*D-021.*JS-148|no task yet.*JS-148.*D-021|D-021.*no task yet.*JS-148|JS-148.*no task yet.*D-021`,
		"that direct play is not capped":     `(?i)direct play`,
		"that a copy is not capped":          `(?i)\bcopy\b`,
		"a copy's fallback (HEVC or AV1)":    `(?i)fallback[^.]*(HEVC|AV1)|(HEVC|AV1)[^.]*fallback`,
		"10 bits for an HDR copy's fallback": `10 bit`,
	} {
		mustMatch(t, "the 1080 line gap", cap, what, pattern)
	}

	p5 := gapWith(t, g, "a re-encoded profile 5 film's picture not observed (JS-149)", `JS-149`)
	for what, pattern := range map[string]string{
		"profile 5":                        `(?i)profile 5`,
		"that its picture is not observed": `(?i)not observed`,
		"that the documents disagreed":     `(?i)disagree`,
		"D-021":                            `D-021`,
		"that it is re-encoded to H.264":   `(?i)re-encoded[^.]*H\.264|H\.264[^.]*re-encoded`,
	} {
		mustMatch(t, "the profile 5 picture gap", p5, what, pattern)
	}
	mustNotMatch(t, "the profile 5 picture gap", p5, "its colours as fact", `(?i)(looks wrong|green and purple|tone mapped from the start)`)

	never := gapWith(t, g, "profile 5 re-encoded, never copied or direct played", `(?i)never copied`)
	for what, pattern := range map[string]string{
		"profile 5":    `(?i)profile 5`,
		"Dolby Vision": `(?i)Dolby Vision`,
		"direct play":  `(?i)direct play`,
		"re-encoded":   `(?i)re-encoded`,
		"a client that does not declare Dolby Vision": `(?i)does not declare Dolby Vision|not declare Dolby Vision|declares? no Dolby Vision`,
	} {
		mustMatch(t, "the profile 5 copy gap", never, what, pattern)
	}
	mustMatch(t, "the profile 5 copy gap", never, "D-021", `D-021`)
	if !closedWithNoTask(never) {
		t.Errorf("the profile 5 copy gap does not say 'no task' (and not 'no task yet'): %s", never)
	}

	// The 401 gap names the clients and requests of the player table.
	auth := gapWith(t, g, "an app whose player sends no token on a media request", `\b401\b`)
	status := docText(t, "docs/status.md")
	noToken := map[string][]string{}
	inTable := false
	for _, l := range strings.Split(status, "\n") {
		if strings.HasPrefix(l, "| Client | Direct play | HLS |") {
			inTable = true
			continue
		}
		if !inTable || !strings.HasPrefix(l, "|") {
			if inTable && l != "" {
				break
			}
			continue
		}
		cells := strings.Split(strings.Trim(l, "| "), " | ")
		if len(cells) < 5 || strings.HasPrefix(cells[0], "---") {
			continue
		}
		for i, kind := range []string{"direct play", "HLS", "subtitle"} {
			if strings.HasPrefix(cells[i+1], "no token") {
				noToken[cells[0]] = append(noToken[cells[0]], kind)
			}
		}
	}
	if len(noToken) == 0 {
		t.Fatal("found no client in the player table whose request carries no token")
	}
	for _, client := range []string{"Findroid", "Android TV", "Swiftfin", "Streamyfin"} {
		kinds, listed := noToken[client]
		if listed != strings.Contains(auth, client) {
			t.Errorf("the 401 gap and the player table disagree about %s: the table lists it as sending no token: %v", client, listed)
		}
		for _, kind := range kinds {
			if !regexp.MustCompile(`(?i)` + kind).MatchString(auth) {
				t.Errorf("the 401 gap does not name %s's %s request, which the player table lists with no token", client, kind)
			}
		}
	}
	mustMatch(t, "the 401 gap", auth, "D-005", `D-005`)
	if !closedWithNoTask(auth) {
		t.Errorf("the 401 gap does not say 'no task' (and not 'no task yet'): %s", auth)
	}

	cue := gapWith(t, g, "seeks into a long-showing cue when a copy ran out its hour", `D-002`)
	for what, pattern := range map[string]string{
		"JS-140, the open task":                    `JS-140`,
		"that a seek costs more the older the cue": `(?i)older|age`,
		"at most once a day":                       `(?i)once a day`,
		"that a play retries it":                   `(?i)\bplay\b`,
		"that a changed file starts it at once":    `(?i)(file|changes?)[^.]*(changes|at once)|at once`,
	} {
		mustMatch(t, "the image subtitle copy gap", cue, what, pattern)
	}

	pg := gapWith(t, g, "Postgres failover", `JS-146`)
	mustMatch(t, "the Postgres failover gap", pg, "that some requests are answered 500", `\b500\b`)
	mustMatch(t, "the Postgres failover gap", pg, "that a subtitle can be refused for 30 s", `(?is)subtitle[^.]*30 ?(s|seconds)|30 ?(s|seconds)[^.]*subtitle`)
}

func TestTheAccessDocNamesTheClientsOfTheStatusTable(t *testing.T) {
	access := docText(t, "docs/access.md")
	for _, l := range linesContaining(access, regexp.MustCompile(`Findroid and Android TV`)) {
		mustMatch(t, "docs/access.md", l, "direct play", `(?i)direct play`)
		mustNotMatch(t, "docs/access.md", l, "Swiftfin or Streamyfin as sending no token", `Swiftfin|Streamyfin`)
		return
	}
	t.Error("docs/access.md does not name Findroid and Android TV as the clients whose direct play sends no token")
}

func TestPostgresFailoverIsNotCalledDoneWithoutItsException(t *testing.T) {
	status := docText(t, "docs/status.md")
	line := lineOf(t, status, "- Postgres failover (JS-029)")
	mustMatch(t, "the Postgres failover line", line, "the gap that JS-146 closes", `JS-146`)
	running := docText(t, "docs/running.md")
	sw := lineOf(t, running, "A Postgres switchover or restart")
	mustMatch(t, "docs/running.md's switchover paragraph", sw, "the gap that JS-146 closes", `JS-146`)
	mustMatch(t, "docs/running.md's switchover paragraph", sw, "that some requests are answered 500 meanwhile", `\b500\b`)
}

// --- the subtitle lines ---

func TestStatusSubtitleLinesSayWhatJS089PutInTheProduct(t *testing.T) {
	status := docText(t, "docs/status.md")
	img := lineOf(t, status, "- Image subtitles inside the file")
	for what, pattern := range map[string]string{
		"JS-089": `JS-089`,
		"that the film is read once for each version of the file": `(?i)(each|every|per) (version of the file|file version)|once for each version`,
		"that the viewer stopping early does not change that":     `(?i)stops? early|stops? after|leaves? (early|after)`,
		"D-003": `D-003`,
		"D-002": `D-002`,
		"that a copy that ran out the hour is retried at most once a day":  `(?is)once a day`,
		"that the cost of a mid-film run does not grow with the cue's age": `(?is)(whatever|not grow|does not grow|regardless)[^.]{0,80}(age|older)`,
	} {
		mustMatch(t, "status.md's image subtitle line", img, what, pattern)
	}
	for _, path := range []string{"docs/status.md", "docs/subtitles.md", "DOMAINS.md"} {
		mustNotMatch(t, path, docText(t, path), "the look-back miss as 'the one remaining gap' beside the listed D-002 gap", `(?i)the one remaining gap`)
	}
	text := lineOf(t, status, "- All text tracks inside a film extracted in one pass")
	mustMatch(t, "status.md's text extraction line", text, "the most reads a run that ends at a limit costs", `2 \+ n`)
}

// subtitleTotal is the one paragraph of docs/subtitles.md that totals the whole-film reads of one film's subtitles.
func subtitleTotal(t *testing.T) string {
	t.Helper()
	var hit []string
	for _, l := range strings.Split(docText(t, "docs/subtitles.md"), "\n") {
		if regexp.MustCompile(`(?i)\btotal`).MatchString(l) && strings.Contains(l, "2 + n") && strings.Contains(l, ".sup") &&
			strings.Contains(l, "D-002") && strings.Contains(l, "D-003") {
			hit = append(hit, l)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("docs/subtitles.md has %d paragraphs that total the whole-film reads of a film's subtitles (naming 2 + n, .sup, D-002 and D-003), want exactly 1", len(hit))
	}
	return hit[0]
}

func TestSubtitlesDocTotalsTheWholeFilmReadsOfOneVersionOfAFile(t *testing.T) {
	p := subtitleTotal(t)
	for what, pattern := range map[string]string{
		"text extraction: the first request for a text track":            `(?is)text (extraction|track)[^.]{0,300}first request`,
		"text extraction: one read for all its text tracks":              `(?is)one read for all (its |the |of its )?text tracks`,
		"two reads when ffmpeg refuses a track":                          `(?is)\btwo\b[^.]{0,80}(refuses|refused)|(refuses|refused)[^.]{0,120}\btwo\b`,
		"at most 2 + n for n text tracks when a run ends at a limit":     `(?is)2 \+ n[^.]{0,150}limit|limit[^.]{0,200}2 \+ n`,
		"each .sup copy: a client that draws PGS asks for a PGS track":   `(?is)\.sup[^.]{0,300}(draws|draw) PGS|(draws|draw) PGS[^.]{0,300}\.sup`,
		"one read for each such PGS track":                               `(?is)one read for each[^.]{0,60}(PGS|\.sup|such) track`,
		"the image-track copy: the first play that burns an image track": `(?is)image[- ]track[^.]{0,300}first play[^.]{0,80}burns|first play[^.]{0,80}burns[^.]{0,80}image track`,
		"also when the viewer stops early":                               `(?is)stops? early`,
		"one read for all its image tracks":                              `(?is)one read for all (its |the |of its )?image tracks`,
		"D-003":                                                          `D-003`,
		"D-002's retry: at most one more read each day":                  `(?is)D-002[^.]{0,300}at most one more read[^.]{0,80}(each|a|per) day|at most one more read[^.]{0,80}(each|a|per) day[^.]{0,300}D-002`,
		"that a replaced file is read again":                             `(?is)(replaced|changed) file[^.]{0,120}(read|counted) again|(read|counted) again[^.]{0,120}(replaced|changed) file|new version[^.]{0,80}read`,
		"what the total does not count (not counted)":                    `(?is)(does not|doesn't|do not) count`,
		"a killed worker":                                                `(?is)killed worker`,
		"a store error":                                                  `(?is)store error`,
		"a lost lease":                                                   `(?is)lost lease`,
	} {
		mustMatch(t, "the total paragraph of docs/subtitles.md", p, what, pattern)
	}
}

func TestEveryDocGivesTheSameCountsAsTheSubtitleTotal(t *testing.T) {
	// A text extraction costs at most 2 + n reads for n tracks; nothing else says 3 + n or 1 + n as the most.
	for _, path := range []string{"docs/subtitles.md", "DOMAINS.md", "docs/running.md", "docs/status.md"} {
		text := docText(t, path)
		for _, m := range regexp.MustCompile(`(\d) \+ n`).FindAllStringSubmatch(text, -1) {
			if m[1] != "1" && m[1] != "2" {
				t.Errorf("%s gives %s + n reads for a film's text tracks; the total says at most 2 + n", path, m[1])
			}
		}
	}
	// The image-track copy costs one read, two when ffmpeg refuses a track: DOMAINS 51 says at most once more.
	p := subtitleTotal(t)
	mustNotMatch(t, "the total paragraph", p, "a count of the image-track copy other than one or two", `(?i)image[- ]tracks?[^.]{0,200}\b(three|four|3|4) reads`)
}

// --- DOMAINS.md ---

func TestDomainsAgreeWithWhatIsBuilt(t *testing.T) {
	domains := docText(t, "DOMAINS.md")
	inv := func(n string) string { return lineOf(t, domains, n+". ") }
	i45, i47, i53 := inv("45"), inv("47"), inv("53")

	mustNotMatch(t, "DOMAINS 45", i45, "no fallback encode for AV1", `none for AV1`)
	mustNotMatch(t, "DOMAINS 45", i45, "that a copy's picture must be SDR", `The picture must be SDR`)
	mustNotMatch(t, "DOMAINS 45", i45, "that the HDR copy is not built", `(?i)(that is|HDR (copy|direct play)[^.]{0,40}is) not built`)
	mustMatch(t, "DOMAINS 45", i45, "that HDR10, HDR10+ and HLG are copied with VIDEO-RANGE=PQ or HLG", `VIDEO-RANGE=PQ`)
	mustMatch(t, "DOMAINS 45", i45, "the AV1 fallback", `(?i)SVT-AV1|AV1 copy goes on as AV1`)

	mustNotMatch(t, "DOMAINS 47", i47, "that every AV1 fallback is 8 bit", `8 bit for an 8 or 10 bit copy`)
	mustMatch(t, "DOMAINS 47", i47, "that an HDR copy's fallback is 10 bit (JS-134)", `(?is)JS-134[^.]{0,300}10 bit|10 bit[^.]{0,300}JS-134`)

	mustNotMatch(t, "DOMAINS 53", i53, "that HDR direct play and copy are not built", `(?i)Not built:[^.]*HDR direct play`)
	mustNotMatch(t, "DOMAINS 53", i53, "that a copy is never offered for an HDR source", `never offers? one for an HDR source|`+"`Decide`"+` never offers a copy for an HDR source|`+"`Decide`"+` never offers one for an HDR source`)
	mustMatch(t, "DOMAINS 53", i53, "that HDR10, HDR10+ and HLG HEVC and AV1 are copied with VIDEO-RANGE=PQ or HLG", `VIDEO-RANGE=(PQ|HLG)`)

	mustNotMatch(t, "DOMAINS.md", domains, "that the api replica half of the gate is not built", `API replica half of the gate is Phase 4 and not built`)
}

// The rule for an HDR film's copy is the one docs/transcoding.md's Video copy list gives: copied to a client whose
// profile shows the range, else re-encoded to H.264 and tone mapped.
func TestDomainsAndTranscodingGiveTheSameCopyRuleForHDRFilms(t *testing.T) {
	transcoding := docText(t, "docs/transcoding.md")
	pic := strings.Join(linesContaining(transcoding, regexp.MustCompile(`^- the picture is SDR`)), "\n")
	for path, text := range map[string]string{
		"docs/transcoding.md": pic,
		"DOMAINS 45":          lineOf(t, docText(t, "DOMAINS.md"), "45. "),
		"DOMAINS 53":          lineOf(t, docText(t, "DOMAINS.md"), "53. "),
	} {
		mustMatch(t, path, text, "that HDR10, HDR10+ and HLG are copied to a client that shows the range", `(?is)HDR10\+?[^.]{0,200}(copied|copy)[^.]{0,200}(shows|show|plays)[^.]{0,60}(range|HDR)|VIDEO-RANGE=PQ[^.]{0,200}HLG`)
	}
}

// --- Dolby Vision profile 5 (D-021) ---

// The places that speak of profile 5: each says it is re-encoded to H.264, that its colours are not observed and
// that JS-149 settles it, and none says as fact that it is tone mapped, looks wrong or comes out in some colours.
func TestEveryPlaceSaysTheSameOfDolbyVisionProfile5(t *testing.T) {
	transcoding := docText(t, "docs/transcoding.md")
	status := docText(t, "docs/status.md")
	places := map[string]string{
		"DOMAINS 53":                                 lineOf(t, docText(t, "DOMAINS.md"), "53. "),
		"transcoding.md Video copy list":             lineOf(t, transcoding, "- the picture is SDR"),
		"transcoding.md HDR copy fallback paragraph": strings.Join(linesContaining(transcoding, regexp.MustCompile(`An HDR copy's fallback keeps the picture's range`)), "\n"),
		"transcoding.md Tone mapping section":        strings.Join(linesContaining(transcoding, regexp.MustCompile(`^- the file declares PQ or HLG`)), "\n"),
		"status.md tone mapping line":                lineOf(t, status, "- HDR tone mapping to SDR"),
	}
	for where, text := range places {
		if text == "" {
			t.Errorf("%s was not found", where)
			continue
		}
		mustMatch(t, where, text, "profile 5 re-encoded to H.264", `(?is)profile 5[^\n]{0,400}re-encoded`)
		mustMatch(t, where, text, "that profile 5's colours are not observed", `(?is)profile 5[^\n]{0,600}not observed`)
		mustMatch(t, where, text, "that JS-149 settles it", `JS-149`)
		mustMatch(t, where, text, "D-021", `D-021`)
	}
	// The profile 5 sentences (and the one after each, which may say "it") say nothing of tone mapping as fact, and
	// whether the worker's tone-mapping filter is applied to profile 5 is said by all of the places or by none, alike.
	places["status.md profile 5 gap"] = gapWith(t, gapSection(t), "the profile 5 picture gap", `JS-149`)
	filterSaid := map[string]string{}
	toneWord := regexp.MustCompile(`(?i)tone[- ]?map`)
	for where, text := range places {
		sentences := regexp.MustCompile(`\.\s+|\n`).Split(text, -1)
		for i, sent := range sentences {
			if !regexp.MustCompile(`(?i)profile 5`).MatchString(sent) {
				continue
			}
			for _, s := range sentences[i:min(i+2, len(sentences))] {
				if !toneWord.MatchString(s) {
					continue
				}
				if !regexp.MustCompile(`(?i)filter`).MatchString(s) {
					t.Errorf("%s says of profile 5 in a sentence about tone mapping that is not the statement about the filter: %q", where, s)
					continue
				}
				if regexp.MustCompile(`(?i)\b(not|never|isn't|no)\b[^,;]{0,40}(applied|run|used)|(applied|run|used)[^,;]{0,20}\bnot\b`).MatchString(s) {
					filterSaid[where] = "not applied"
				} else {
					filterSaid[where] = "applied"
				}
			}
		}
	}
	if len(filterSaid) != 0 && len(filterSaid) != len(places) {
		t.Errorf("only %d of %d places say whether the tone-mapping filter is applied to profile 5: %v (all or none)", len(filterSaid), len(places), filterSaid)
	}
	distinct := map[string]bool{}
	for _, v := range filterSaid {
		distinct[v] = true
	}
	if len(distinct) > 1 {
		t.Errorf("the places disagree whether the tone-mapping filter is applied to profile 5: %v", filterSaid)
	}
	for _, path := range []string{"DOMAINS.md", "docs/transcoding.md", "docs/status.md", "docs/running.md", "docs/access.md", "docs/subtitles.md"} {
		text := docText(t, path)
		for what, pattern := range map[string]string{
			"profile 5 as tone mapped from the start": `(?i)tone mapped from the start`,
			"profile 5 as looking wrong":              `(?i)(profile 5[^\n]{0,200}(looks|look) wrong|still looks wrong)`,
			"the colours profile 5 comes out in":      `(?i)green and purple|green and magenta`,
			"profile 5 as not mapped":                 `(?i)profile 5[^\n]{0,120}(is not mapped|is left alone|is left as it was)`,
		} {
			mustNotMatch(t, path, text, what, pattern)
		}
	}
}

// --- docs/transcoding.md and docs/running.md ---

func TestTranscodingAndRunningDocsNameTheAV1Fallback(t *testing.T) {
	mustNotMatch(t, "docs/transcoding.md", docText(t, "docs/transcoding.md"), "that the AV1 fallback is not built", `(?i)AV1 fallback is not built`)
	running := docText(t, "docs/running.md")
	row := lineOf(t, running, "| `jellyscale_playback_copy_fallbacks_total`")
	mustMatch(t, "docs/running.md's jellyscale_playback_copy_fallbacks_total", row, "AV1 (SVT-AV1) beside x264 and x265", `(?is)x264[^|]{0,40}x265[^|]{0,40}(AV1|SVT-AV1)`)
}

// --- the captures ---

type av1Cell struct {
	Frames                                       float64
	VideoError                                   *string
	Dialog                                       bool
	MaxBackSeconds, MaxForwardBeyondClockSeconds float64
	Colours                                      struct{ Before, After []float64 }
	Reached                                      float64
	FatalErrors                                  int
}

func tableRows(t *testing.T, md, header string) [][]string {
	t.Helper()
	var rows [][]string
	in := false
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, header) {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(l, "|") {
			break
		}
		cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(l, "|"), "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if strings.HasPrefix(cells[0], "---") {
			continue
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		t.Fatalf("no table starts %q", header)
	}
	return rows
}

func cellOf(t *testing.T, raw json.RawMessage) av1Cell {
	t.Helper()
	var c av1Cell
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// compareRows holds each row of a record's table to the cell its JSON holds: frames shown, video.error, dialog and
// whether the position is kept, and for the HDR record the colours.
func compareRows(t *testing.T, md, header string, label func(string) string, observed map[string]map[string]json.RawMessage, colours bool) {
	t.Helper()
	cur := ""
	for _, r := range tableRows(t, md, header) {
		if r[0] != "" {
			cur = label(r[0])
		}
		dir := "forward"
		if strings.HasPrefix(r[1], "back") {
			dir = "back"
		}
		raw, ok := observed[cur][dir]
		if !ok {
			t.Errorf("the table has a row %q (%s, %s) that the JSON does not hold", r[0], cur, dir)
			continue
		}
		c := cellOf(t, raw)
		where := cur + " " + dir

		first := regexp.MustCompile(`\d+`).FindString(r[2])
		if n, _ := strconv.Atoi(first); float64(n) != c.Frames {
			t.Errorf("%s: frames shown: the record says %q, the JSON holds %v", where, r[2], c.Frames)
		}
		if c.VideoError == nil && !strings.HasPrefix(r[3], "none") {
			t.Errorf("%s: video.error: the record says %q, the stored run has none", where, r[3])
		}
		if c.VideoError != nil && !strings.Contains(r[3], "CHUNK_DEMUXER_ERROR_APPEND_FAILED") && !strings.HasPrefix(r[3], "the same") {
			t.Errorf("%s: video.error: the record says %q, the JSON holds %q", where, r[3], *c.VideoError)
		}
		if got := strings.HasPrefix(r[4], "yes"); got != c.Dialog {
			t.Errorf("%s: dialog: the record says %q, the JSON holds %v", where, r[4], c.Dialog)
		}
		kept := c.MaxBackSeconds == 0 && c.MaxForwardBeyondClockSeconds == 0
		if got := strings.HasPrefix(r[5], "yes"); got != kept {
			t.Errorf("%s: position kept: the record says %q, the JSON (maxBackSeconds %v, maxForwardBeyondClockSeconds %v) gives %v", where, r[5], c.MaxBackSeconds, c.MaxForwardBeyondClockSeconds, kept)
		}
		if colours && !strings.HasPrefix(r[6], "the same") {
			var said []float64
			for _, n := range regexp.MustCompile(`\d+(\.\d+)?`).FindAllString(r[6], -1) {
				f, _ := strconv.ParseFloat(n, 64)
				said = append(said, f)
			}
			want := append(append([]float64{}, c.Colours.Before...), c.Colours.After...)
			if len(said) != len(want) {
				t.Errorf("%s: colours: the record says %q, the JSON holds %v", where, r[6], want)
				continue
			}
			for i := range want {
				if said[i] != want[i] {
					t.Errorf("%s: colours: the record says %q, the JSON holds %v", where, r[6], want)
					break
				}
			}
		}
	}
}

func TestAV1FallbackRecordAgreesWithItsJSONCellForCell(t *testing.T) {
	md := docText(t, "compat/captures/av1-fallback-12.4.md")
	var rec struct {
		Choice   string
		Note     string
		Observed map[string]map[string]json.RawMessage
	}
	if err := json.Unmarshal([]byte(docText(t, "compat/captures/av1-fallback-12.4.json")), &rec); err != nil {
		t.Fatal(err)
	}
	compareRows(t, md, "| the later segments are |", func(s string) string {
		switch {
		case strings.HasPrefix(s, "AV1, no change"):
			return "control"
		case strings.HasPrefix(s, "AV1"):
			return "av1"
		}
		return "h264"
	}, rec.Observed, false)

	// A run the JSON does not hold is said to be not stored, in the record and in the JSON's note.
	for where, text := range map[string]string{"the record": md, "the JSON's note": rec.Note} {
		for _, m := range regexp.MustCompile(`(?i)first run|a rerun`).FindAllStringIndex(text, -1) {
			lo, hi := max(0, m[0]-400), min(len(text), m[1]+400)
			if !regexp.MustCompile(`(?i)not stored`).MatchString(text[lo:hi]) {
				t.Errorf("%s mentions %q without saying that run is not stored", where, text[m[0]:m[1]])
			}
		}
	}
	if rec.Choice != "av1" {
		t.Errorf("the choice in the JSON is %q, want av1", rec.Choice)
	}
	mustMatch(t, "the record", md, "that the fallback of an AV1 copy is AV1", `(?s)The fallback of an AV1 copy is AV1 \(SVT-AV1\)`)
}

func TestTheTenBitAV1ObservationIsStoredWithTheFieldsOfTheEightBitRuns(t *testing.T) {
	md := docText(t, "compat/captures/av1-fallback-12.4.md")
	var rec struct {
		Observed map[string]map[string]json.RawMessage
	}
	if err := json.Unmarshal([]byte(docText(t, "compat/captures/av1-fallback-12.4.json")), &rec); err != nil {
		t.Fatal(err)
	}
	var ten map[string]json.RawMessage
	for k, v := range rec.Observed {
		if regexp.MustCompile(`(?i)10|ten`).MatchString(k) {
			ten = v
		}
	}
	if ten == nil {
		t.Fatal("av1-fallback-12.4.json has no 10-bit run among its observed runs (a key naming 10 or ten)")
	}
	keys := func(raw json.RawMessage) string {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		return strings.Join(sortedStrings(ks), ",")
	}
	for _, dir := range []string{"forward", "back"} {
		if keys(ten[dir]) != keys(rec.Observed["av1"][dir]) {
			t.Errorf("the 10-bit %s run has fields %q, the 8-bit runs have %q", dir, keys(ten[dir]), keys(rec.Observed["av1"][dir]))
		}
	}
	section := md[strings.Index(md, "## A 10-bit AV1 copy"):]
	mustNotMatch(t, "the 10-bit section", section, "that its numbers come from a run that was not stored", `(?i)not stored`)
	mustMatch(t, "the 10-bit section", section, "forward and then back and forward", `(?i)forward[^.]*back and forward|back and forward`)
	f, b := cellOf(t, ten["forward"]).Frames, cellOf(t, ten["back"]).Frames
	mustMatch(t, "the 10-bit section", section, "the stored frames, forward", `\b`+strconv.Itoa(int(f))+`\b`)
	mustMatch(t, "the 10-bit section", section, "the stored frames, back and forward", `\b`+strconv.Itoa(int(b))+`\b`)
	// The run is made by the script kept beside the JSON, not by an edit of it described in prose.
	script := docText(t, "compat/captures/av1-fallback-spike.mjs")
	mustMatch(t, "av1-fallback-spike.mjs", script, "the 10-bit codec string it runs", `av01\.0\.04M\.10`)
	mustMatch(t, "av1-fallback-spike.mjs", script, "the 10-bit pixel format of the stream it plays", `yuv420p10le|10-?bit`)
}

func sortedStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

func TestHDRFallbackRecordAgreesWithItsJSON(t *testing.T) {
	md := docText(t, "compat/captures/hdr-fallback-12.4.md")
	var rec struct {
		Observed struct {
			AV1 map[string]map[string]json.RawMessage `json:"av1"`
		}
	}
	if err := json.Unmarshal([]byte(docText(t, "compat/captures/hdr-fallback-12.4.json")), &rec); err != nil {
		t.Fatal(err)
	}
	compareRows(t, md, "| the later segments are |", func(s string) string {
		switch {
		case strings.HasPrefix(s, "the copy all"):
			return "control"
		case strings.HasPrefix(s, "today's"):
			return "eight"
		}
		return "ten"
	}, rec.Observed.AV1, true)

	// The tone-mapped fallback's shift: its mean RGB after the change minus before, per channel, as the JSON holds it.
	eight := cellOf(t, rec.Observed.AV1["eight"]["forward"]).Colours
	lo, hi := 1e9, -1e9
	for i := range eight.Before {
		d := eight.After[i] - eight.Before[i]
		lo, hi = min(lo, d), max(hi, d)
	}
	want := strconv.FormatFloat(lo, 'f', 1, 64) + " to " + strconv.FormatFloat(hi, 'f', 1, 64) + " units"
	if !strings.Contains(md, want) {
		t.Errorf("the record does not give the tone-mapped fallback's shift as %q (its JSON holds before %v, after %v)", want, eight.Before, eight.After)
	}
	mustNotMatch(t, "the record", md, "a shift of 3 to 5 units", `3 to 5 units`)
}

func TestHDRFallbackRecordSaysTheBrowserObservationHadOneTimescale(t *testing.T) {
	md := docText(t, "compat/captures/hdr-fallback-12.4.md")
	var conclusion string
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "The player takes") {
			conclusion = l
		}
	}
	if conclusion == "" {
		t.Fatal("the record has no paragraph that starts 'The player takes', where it concludes what the player takes")
	}
	mustMatch(t, "the paragraph where the record concludes what the player takes", conclusion, "that the copy and the fallback had one video timescale", `(?is)(one|same|equal|identical)[^.]{0,60}(video )?timescale|timescale[^.]{0,60}(one|same|equal|identical)`)
	mustMatch(t, "the record", md, "that with unequal timescales the synthetic streams stalled", `(?is)(unequal|different|differ)[^.]{0,200}stall|stall[^.]{0,200}(unequal|different|differ)|timescale[^.]{0,300}stalled`)
	mustNotMatch(t, "the record", md, "the browser observation as one of unequal timescales", `(?is)(observed|table)[^.]{0,120}(unequal|different) (video )?timescales?`)
}
