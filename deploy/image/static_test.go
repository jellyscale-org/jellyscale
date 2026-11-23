package image

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// JS-124: the compose file, the Dockerfile and test/image.sh, read as
// text. None needs Docker or ffmpeg.

const (
	composePath = "deploy/fixtures/compose.yaml"
	expectPath  = "test/image-expect.tsv"
	imageName   = "ghcr.io/jellyscale-org/jellyscale"
)

// --- deploy/fixtures/compose.yaml

func TestFixturesComposeIsTwoServicesAndNothingElse(t *testing.T) {
	doc := parse(t, repoFile(t, composePath))
	services, _ := doc["services"].(map[string]any)
	if len(services) != 2 {
		t.Fatalf("want exactly two services (Postgres and the fixtures image), got %d", len(services))
	}
	for key := range doc {
		if key != "services" && key != "name" {
			t.Errorf("top-level key %q: a client's file has no volumes, networks, configs or secrets of its own", key)
		}
	}
	var server, postgres map[string]any
	for _, v := range services {
		svc := v.(map[string]any)
		for _, banned := range []string{"build", "volumes", "env_file", "command", "container_name", "entrypoint", "init", "profiles", "extends"} {
			if _, ok := svc[banned]; ok {
				t.Errorf("a service has %q", banned)
			}
		}
		if strings.Contains(text(svc["image"]), "JELLYSCALE_IMAGE") {
			server = svc
		} else if strings.HasPrefix(text(svc["image"]), "postgres") {
			postgres = svc
		}
	}
	if server == nil || postgres == nil {
		t.Fatal("one service must be image: ${JELLYSCALE_IMAGE:?...} and the other a postgres image")
	}
	if !strings.Contains(text(server["image"]), "${JELLYSCALE_IMAGE:?") {
		t.Errorf("image is %q: it must use ${JELLYSCALE_IMAGE:?message}, which fails naming the variable when it is unset or empty", server["image"])
	}
	if _, ok := postgres["ports"]; ok {
		t.Error("Postgres publishes a port")
	}
	if _, ok := postgres["expose"]; ok {
		t.Log("Postgres exposes a port inside the network (allowed)")
	}
}

func TestFixturesComposePublishesTheServerOnLoopbackAtAPortDockerAssigns(t *testing.T) {
	doc := parse(t, repoFile(t, composePath))
	for _, v := range doc["services"].(map[string]any) {
		svc := v.(map[string]any)
		if !strings.Contains(text(svc["image"]), "JELLYSCALE_IMAGE") {
			continue
		}
		ports, _ := svc["ports"].([]any)
		if len(ports) != 1 {
			t.Fatalf("want one published port, got %v", svc["ports"])
		}
		switch p := ports[0].(type) {
		case string:
			if p != "127.0.0.1::8096" {
				t.Errorf("port %q, want 127.0.0.1::8096 (loopback, no host port, the server's own 8096)", p)
			}
		case map[string]any:
			if text(p["host_ip"]) != "127.0.0.1" || p["published"] != nil || text(p["target"]) != "8096" {
				t.Errorf("port %v, want host_ip 127.0.0.1, target 8096 and no published port", p)
			}
		default:
			t.Errorf("port %v", p)
		}
	}
}

func TestFixturesComposeIsHealthyWhenReadyzAnswers200(t *testing.T) {
	doc := parse(t, repoFile(t, composePath))
	for _, v := range doc["services"].(map[string]any) {
		svc := v.(map[string]any)
		hc, _ := svc["healthcheck"].(map[string]any)
		if hc == nil {
			t.Errorf("service %v has no healthcheck: up --wait would not wait for it", svc["image"])
			continue
		}
		if strings.Contains(text(svc["image"]), "JELLYSCALE_IMAGE") {
			test := text(hc["test"])
			if !strings.Contains(test, "/readyz") || strings.Contains(test, "/healthz") {
				t.Errorf("the server's healthcheck is %q: it must ask /readyz (which needs the database), not /healthz", test)
			}
			if strings.Contains(test, "wget") && !strings.Contains(test, "-q") && !strings.Contains(test, "--spider") {
				t.Logf("healthcheck %q", test)
			}
			dep, _ := svc["depends_on"].(map[string]any)
			if dep == nil || text(dep["postgres"]) == "" && len(dep) == 0 {
				t.Error("the server does not depend_on Postgres being healthy")
			}
			for _, d := range dep {
				if m, ok := d.(map[string]any); !ok || text(m["condition"]) != "service_healthy" {
					t.Errorf("depends_on %v: want condition service_healthy", d)
				}
			}
		}
	}
}

func TestFixturesComposeHoldsTheTestPasswordAndTheDatabaseAndNothingMore(t *testing.T) {
	raw := repoFile(t, composePath)
	doc := parse(t, raw)
	for _, v := range doc["services"].(map[string]any) {
		svc := v.(map[string]any)
		if !strings.Contains(text(svc["image"]), "JELLYSCALE_IMAGE") {
			continue
		}
		env := map[string]string{}
		switch e := svc["environment"].(type) {
		case map[string]any:
			for k, val := range e {
				env[k] = text(val)
			}
		case []any:
			for _, item := range e {
				k, val, _ := strings.Cut(text(item), "=")
				env[k] = val
			}
		}
		if env["JELLYSCALE_ADMIN_PASSWORD"] != "test" {
			t.Errorf("JELLYSCALE_ADMIN_PASSWORD is %q in the file, want test", env["JELLYSCALE_ADMIN_PASSWORD"])
		}
		if !strings.HasPrefix(env["JELLYSCALE_DATABASE_URL"], "postgres") {
			t.Errorf("JELLYSCALE_DATABASE_URL is %q", env["JELLYSCALE_DATABASE_URL"])
		}
		for k := range env {
			if k != "JELLYSCALE_ADMIN_PASSWORD" && k != "JELLYSCALE_DATABASE_URL" {
				t.Errorf("the server gets %s: the image alone carries the rest", k)
			}
		}
	}
}

// --- the Dockerfile

type stage struct {
	name, from string
	lines      []string
}

func dockerfile(t *testing.T) []stage {
	t.Helper()
	var stages []stage
	from := regexp.MustCompile(`(?i)^FROM\s+(?:--\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?`)
	sc := bufio.NewScanner(strings.NewReader(strings.ReplaceAll(repoFile(t, "Dockerfile"), "\\\n", " ")))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := from.FindStringSubmatch(line); m != nil {
			stages = append(stages, stage{name: m[2], from: m[1]})
			continue
		}
		if len(stages) > 0 {
			stages[len(stages)-1].lines = append(stages[len(stages)-1].lines, line)
		}
	}
	return stages
}

func TestDockerfileHasAServerTargetLastAndAFixturesTargetOnTheSameBase(t *testing.T) {
	stages := dockerfile(t)
	if len(stages) == 0 || stages[len(stages)-1].name != "server" {
		t.Fatal("the last stage must be named server: a build that names no target (deploy/compose/compose.yaml's) builds the last stage and must still give the server image")
	}
	var fixtures, server *stage
	for i := range stages {
		switch stages[i].name {
		case "fixtures":
			fixtures = &stages[i]
		case "server":
			server = &stages[i]
		}
	}
	if fixtures == nil {
		t.Fatal("no stage named fixtures")
	}
	if fixtures.from != server.from {
		t.Errorf("fixtures is FROM %s and server FROM %s: the fixtures image is the server image plus layers, so they share a base (same binary, same ffmpeg, same user)", fixtures.from, server.from)
	}
	var base *stage
	for i := range stages {
		if stages[i].name == server.from {
			base = &stages[i]
		}
	}
	all := append(append([]string{}, server.lines...), fixtures.lines...)
	if base != nil {
		all = append(all, base.lines...)
	}
	joined := strings.Join(all, "\n")
	if !regexp.MustCompile(`(?m)^ENTRYPOINT \["jellyscale"\]$`).MatchString(joined) {
		t.Error(`ENTRYPOINT must be exactly ["jellyscale"] with no default arguments`)
	}
	if !regexp.MustCompile(`(?m)^USER 1000:1000$`).MatchString(joined) {
		t.Error("USER 1000:1000 is missing from the server or its base")
	}
	inServer := append([]string{}, server.lines...)
	if base != nil {
		inServer = append(inServer, base.lines...)
	}
	for _, l := range inServer {
		if strings.HasPrefix(strings.ToUpper(l), "CMD") {
			t.Errorf("the server image has a default argument: %s", l)
		}
		if strings.Contains(l, "/fixtures") {
			t.Errorf("the server image holds fixtures: %s", l)
		}
	}
}

func TestDockerfileFixturesStageCopiesFilmsAndAConfigAndMakesNothing(t *testing.T) {
	var fixtures *stage
	stages := dockerfile(t)
	for i := range stages {
		if stages[i].name == "fixtures" {
			fixtures = &stages[i]
		}
	}
	if fixtures == nil {
		t.Fatal("no stage named fixtures")
	}
	var cmd, copied string
	media := false
	for _, l := range fixtures.lines {
		u := strings.ToUpper(l)
		switch {
		case strings.HasPrefix(u, "RUN"):
			if regexp.MustCompile(`ffmpeg|ffprobe|client\.sh|make\.sh|testsrc|go build|libx26`).MatchString(l) {
				t.Errorf("a fixtures step makes or re-encodes a film: %s", l)
			}
		case strings.HasPrefix(u, "CMD"):
			cmd = l
		case strings.HasPrefix(u, "COPY"):
			if strings.Contains(l, "--from=") {
				t.Errorf("fixtures copies from another stage (a film is made outside the image, on the runner): %s", l)
			}
			if strings.HasSuffix(l, " /fixtures/media") || strings.HasSuffix(l, " /fixtures/media/") {
				media = true
			}
			if strings.HasSuffix(l, ".json") || strings.Contains(l, ".json ") {
				copied = l
			}
		case strings.HasPrefix(u, "USER"):
			if !strings.Contains(l, "1000:1000") {
				t.Errorf("fixtures changes the user: %s", l)
			}
		}
	}
	if !media {
		t.Error("no COPY into /fixtures/media")
	}
	if copied == "" {
		t.Error("no COPY of the config (a .json file)")
	}
	if !strings.Contains(cmd, "--roles=all") || !regexp.MustCompile(`--config[= ]\S*/fixtures/\S+\.json|/fixtures/\S+\.json`).MatchString(cmd) || !strings.Contains(cmd, "--config") {
		t.Errorf("CMD %q: the fixtures image starts as --roles=all with its config under /fixtures with no arguments", cmd)
	}
	whole := repoFile(t, "Dockerfile")
	for _, banned := range []string{"jellyfin-web", "web-dist", "adminPassword", "JELLYSCALE_ADMIN_PASSWORD", "JELLYSCALE_DATABASE_URL"} {
		if strings.Contains(whole, banned) {
			t.Errorf("the Dockerfile mentions %s", banned)
		}
	}
	if regexp.MustCompile(`(?im)^(ENV|ARG)\s+\S*(PASSWORD|SECRET|TOKEN)`).MatchString(whole) {
		t.Error("the Dockerfile sets a password, secret or token as ENV or ARG (it would be in the history)")
	}
}

func TestFixturesConfigNamesLibrariesAndAdminAndHasNoPasswordOrWebDir(t *testing.T) {
	var cfg string
	var fixtures *stage
	stages := dockerfile(t)
	for i := range stages {
		if stages[i].name == "fixtures" {
			fixtures = &stages[i]
		}
	}
	if fixtures == nil {
		t.Fatal("no stage named fixtures")
	}
	for _, l := range fixtures.lines {
		if m := regexp.MustCompile(`(?i)^COPY\s+(?:--\S+\s+)*(\S+\.json)\s`).FindStringSubmatch(l); m != nil {
			cfg = m[1]
		}
	}
	if cfg == "" {
		t.Fatal("the fixtures stage copies no .json config")
	}
	raw := repoFile(t, cfg)
	doc := parseJSON(t, raw)
	if doc["adminUser"] != "test" {
		t.Errorf("adminUser is %v, want test", doc["adminUser"])
	}
	for _, key := range []string{"adminPassword", "webDir"} {
		if _, ok := doc[key]; ok {
			t.Errorf("the config has %s", key)
		}
	}
	libs, _ := doc["libraries"].([]any)
	if len(libs) == 0 {
		t.Fatal("the config names no library")
	}
	expect := readExpect(t)
	if len(libs) != len(expect.libraries) {
		t.Errorf("the config has %d libraries, %s has %d", len(libs), expectPath, len(expect.libraries))
	}
	for _, l := range libs {
		m := l.(map[string]any)
		if !strings.HasPrefix(text(m["media"]), "file:///fixtures/media/") {
			t.Errorf("library %v: media %v is not under /fixtures/media", m["name"], m["media"])
		}
		if strings.Contains(text(m["assets"]), "/fixtures") {
			t.Errorf("library %v: assets %v are inside the read-only film tree; they must be written inside the container elsewhere", m["name"], m["assets"])
		}
	}
	for _, key := range []string{"transcodeStore"} {
		if v := text(doc[key]); doc[key] == nil || strings.Contains(v, "/fixtures") {
			t.Errorf("%s is %v: segments need a place inside the container outside /fixtures (the transcode check makes one)", key, doc[key])
		}
	}
	// A writable place for assets and segments must exist for the non-root user.
	if !strings.Contains(strings.Join(fixtures.lines, "\n"), "1000") && !strings.Contains(strings.Join(fixtures.lines, "\n"), "chown") {
		t.Log("fixtures stage does not mention how 1000:1000 may write the assets and segments; image.sh's transcode check shows it")
	}
}

func TestOnlyTheComposeFileHoldsThePassword(t *testing.T) {
	root := filepath.Join("..", "..")
	var found []string
	err := filepath.WalkDir(filepath.Join(root, "deploy", "fixtures"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, _ := os.ReadFile(path)
		if regexp.MustCompile(`(?i)password`).Match(raw) && filepath.Base(path) != "compose.yaml" {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("files other than compose.yaml name a password: %v", found)
	}
	if !strings.Contains(repoFile(t, composePath), "JELLYSCALE_ADMIN_PASSWORD") {
		t.Error("compose.yaml does not set JELLYSCALE_ADMIN_PASSWORD")
	}
}

// --- test/image-expect.tsv, which test/image.sh reads

type track struct {
	index             int
	kind, lang, codec string
	flags             string // "default,forced,external" in that order, "-" for none
}

type expectation struct {
	image, compose, user, password string
	films, episodes                []string
	filmCount, episodeCount        int
	libraries                      []string // name, kind, folder joined by tab
	tracks                         map[string][]track
	plays                          map[string]string // film to "direct" or "transcode"
}

func readExpect(t *testing.T) expectation {
	t.Helper()
	e := expectation{tracks: map[string][]track{}, plays: map[string]string{}}
	for n, line := range strings.Split(repoFile(t, expectPath), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		bad := func() { t.Fatalf("%s:%d: %q is not a tab-separated line of a known kind", expectPath, n+1, line) }
		switch f[0] {
		case "image", "compose", "user", "password", "films", "episodes":
			if len(f) != 2 {
				bad()
			}
			switch f[0] {
			case "image":
				e.image = f[1]
			case "compose":
				e.compose = f[1]
			case "user":
				e.user = f[1]
			case "password":
				e.password = f[1]
			case "films", "episodes":
				v, err := strconv.Atoi(f[1])
				if err != nil {
					bad()
				}
				if f[0] == "films" {
					e.filmCount = v
				} else {
					e.episodeCount = v
				}
			}
		case "library":
			if len(f) != 4 {
				bad()
			}
			e.libraries = append(e.libraries, strings.Join(f[1:], "\t"))
		case "film":
			if len(f) != 2 {
				bad()
			}
			e.films = append(e.films, f[1])
		case "episode":
			if len(f) != 2 {
				bad()
			}
			e.episodes = append(e.episodes, f[1])
		case "plays":
			if len(f) != 3 || f[2] != "direct" && f[2] != "transcode" {
				bad()
			}
			e.plays[f[1]] = f[2]
		case "track":
			if len(f) != 7 {
				bad()
			}
			i, err := strconv.Atoi(f[2])
			if err != nil {
				bad()
			}
			e.tracks[f[1]] = append(e.tracks[f[1]], track{i, f[3], f[4], f[5], f[6]})
		default:
			bad()
		}
	}
	return e
}

// The tracks the criteria give, as PlaybackInfo lists them.
var babel = []track{
	{0, "Video", "none", "h264", "-"},
	{1, "Audio", "eng", "aac", "default"},
	{2, "Audio", "deu", "aac", "-"},
	{3, "Audio", "fra", "aac", "-"},
	{4, "Audio", "none", "aac", "-"},
	{5, "Subtitle", "eng", "subrip", "-"},
	{6, "Subtitle", "eng", "subrip", "forced"},
	{7, "Subtitle", "eng", "subrip", "-"},
	{8, "Subtitle", "deu", "subrip", "-"},
	{9, "Subtitle", "ger", "subrip", "forced"},
	{10, "Subtitle", "fra", "subrip", "-"},
	{11, "Subtitle", "none", "subrip", "-"},
	{12, "Subtitle", "jpn", "PGSSUB", "-"},
	{13, "Subtitle", "deu", "subrip", "external"},
	{14, "Subtitle", "spa", "subrip", "external"},
}

var kino = []track{
	{0, "Video", "none", "h264", "-"},
	{1, "Audio", "deu", "aac", "default"},
	{2, "Audio", "eng", "aac", "-"},
	{3, "Subtitle", "deu", "subrip", "-"},
	{4, "Subtitle", "eng", "subrip", "-"},
	{5, "Subtitle", "fra", "subrip", "-"},
}

// PlaybackInfo names a PGS track PGSSUB, as Jellyfin does; ffprobe's hdmv_pgs_subtitle is pinned only in the check of the file.
// The documented codec of the audio and text subtitles of Babel and Kino may be
// another (ac3, ass); what is pinned is type, language, flags and the PGS codec.
func sameTrack(want, got track) bool {
	return want.index == got.index && want.kind == got.kind && want.lang == got.lang && want.flags == got.flags &&
		(want.codec != "PGSSUB" || got.codec == want.codec) && got.codec != ""
}

func TestExpectationHoldsTheCountsAndTheTwoLanguageFilmsExactly(t *testing.T) {
	e := readExpect(t)
	if e.filmCount != 24 || e.episodeCount != 3 || len(e.films) != 24 || len(e.episodes) != 3 {
		t.Errorf("counts: films %d (%d named), episodes %d (%d named); want 24 and 3", e.filmCount, len(e.films), e.episodeCount, len(e.episodes))
	}
	seen := map[string]bool{}
	for _, f := range append(append([]string{}, e.films...), e.episodes...) {
		if seen[f] {
			t.Errorf("%q is named twice: each name must find exactly one item", f)
		}
		seen[f] = true
	}
	for name, want := range map[string][]track{"Babel Film": babel, "Kino Film": kino} {
		got := e.tracks[name]
		if len(got) != len(want) {
			t.Errorf("%s has %d tracks in %s, want %d", name, len(got), expectPath, len(want))
			continue
		}
		for i := range want {
			if !sameTrack(want[i], got[i]) {
				t.Errorf("%s track %d is %v, want %v", name, i, got[i], want[i])
			}
		}
	}
	for _, f := range e.films {
		tr := e.tracks[f]
		if len(tr) == 0 || tr[0].kind != "Video" || tr[0].index != 0 {
			t.Errorf("film %q: its tracks must start with Video at 0", f)
		}
		for i, x := range tr {
			if x.index != i || x.codec == "" || x.flags == "" {
				t.Errorf("film %q track %d is %v: indexes run from 0 without a gap, with a codec and flags (- for none)", f, i, x)
			}
		}
	}
	transcodes := 0
	for _, how := range e.plays {
		if how == "transcode" {
			transcodes++
		}
	}
	if transcodes != 4 {
		t.Errorf("%d films are marked transcode, want make.sh's four AC3 films", transcodes)
	}
	if e.user != "test" || e.password != "test" || e.image != imageName || e.compose != composePath {
		t.Errorf("user %q password %q image %q compose %q", e.user, e.password, e.image, e.compose)
	}
	if _, err := os.Stat(filepath.Join("..", "..", e.compose)); err != nil {
		t.Errorf("the compose path does not exist: %v", err)
	}
}

// --- test/image.sh

func TestImageShReadsTheExpectationAndChecksWhatTheCriteriaName(t *testing.T) {
	sh := repoFile(t, "test/image.sh")
	for _, want := range []string{
		"image-expect.tsv", "org.opencontainers.image.revision", "org.opencontainers.image.source", "org.opencontainers.image.version",
		"docker compose port", "up -d --wait", "/readyz", "PlaybackInfo", "TranscodingUrl", "Stream.vtt", "WEBVTT", "206", "Content-Range",
		"client.sh", "trap", "INT", "TERM",
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("test/image.sh never mentions %q", want)
		}
	}
	if regexp.MustCompile(`(?i)\bskip`).MatchString(sh) {
		t.Error("test/image.sh mentions skipping: a missing tool fails")
	}
}

func parseJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yamlJSON(raw, &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	return doc
}
