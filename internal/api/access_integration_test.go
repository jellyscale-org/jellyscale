//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// JS-125. TestAccess asks every route of Routes() and holds each to what it declares: a 'viewer' route answers the
// same 401 to a request with no token, an unknown token, and a token that was accepted until it was signed out, its
// user was deleted or its device signed in again, in each of the seven accSpellings; an 'anyone' route never answers 401
// or 403; with a valid token no viewer route answers 401. It runs on a accHandler with both roles, then the api routes on
// one with the api alone and the stream routes on one with the stream alone, because a route is served by whichever
// process has its role (test/roles.sh runs them as real processes).
//
// The answers of the routes with a valid token are not checked beyond "not 401" (they are other tests' to check), but
// the media the fixture holds is asked for for real below, so that a 401 can never be a route that is broken for
// everybody.

const (
	accessUserName     = "viewer"
	accessUserPassword = "secret"
)

// accessFilm is one synthetic film: a text subtitle, a font and chapter pictures, in a library of one store.
type accessFilm struct {
	store  string // "posix" or "s3"
	item   string
	source string // Compact, as the paths name it.
	body   []byte
}

type accessSite struct {
	*env
	films   []accessFilm
	hashed  string
	asked   map[string]int // "<accHandler> <method> <path>" to how many requests.
	problem []string
	scoped  *scopeFixture // The users of the JS-127 sweep, made by the first handler that asks.
}

func (a *accessSite) fail(format string, args ...any) {
	a.problem = append(a.problem, fmt.Sprintf(format, args...))
}

func (a *accessSite) report(t *testing.T) {
	t.Helper()
	if len(a.problem) == 0 {
		return
	}
	shown := a.problem[:min(len(a.problem), 25)]
	t.Errorf("%d answers were wrong; the first %d:\n  %s", len(a.problem), len(shown), strings.Join(shown, "\n  "))
	a.problem = nil
}

const accessStreams = `[{"Index":0,"Type":"Video","Codec":"h264","Profile":"Main","Level":41,"VideoRangeType":"SDR","Width":1280,"Height":720,"AverageFrameRate":24},
	{"Index":1,"Type":"Audio","Codec":"aac","IsDefault":true,"Channels":2},
	{"Index":3,"Type":"Subtitle","Codec":"subrip","IsTextSubtitleStream":true},
	{"Index":6,"Type":"Attachment","Codec":"ttf","MimeType":"font/ttf"}]`

var accessFont = bytes.Repeat([]byte("font-six!"), 400)
var accessPicture = append([]byte("\xff\xd8 not decoded by the server "), bytes.Repeat([]byte("x"), 100)...)

// newAccessSite is the test's env (a film in a POSIX library) and a second film in a library on S3, each with a text
// subtitle track and a font in its asset store (so no job is needed to serve them), a poster and chapter pictures.
func newAccessSite(t *testing.T) *accessSite {
	t.Helper()
	e := newEnv(t)
	hash, err := bcrypt.GenerateFromPassword([]byte(accessUserPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.pool, "UPDATE users SET password_hash = $2 WHERE id = $1", e.user, string(hash))
	a := &accessSite{env: e, hashed: string(hash), asked: map[string]int{}}
	a.films = append(a.films, accessFilm{store: "posix", item: e.item, source: db.CompactID(e.item), body: e.body})

	// The same film on the first S3 target, in a library of its own.
	uri := putOnS3(t, s3Targets(t)[0], e.body)
	library, item := uuid.NewString(), uuid.NewString()
	mustExec(t, e.pool, `INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, 'S3', 'movies', $2, $2)`, library, uri)
	mustExec(t, e.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		VALUES ($1, $2, 'Movie', 'Film on S3', 'film on s3', 'film on s3', $3, $4)`, item, library, filmTicks, fmt.Sprintf(`{"Name":"Film on S3","MediaSources":[{
		"Id":%q,"Protocol":"File","Container":"mkv","SupportsTranscoding":true,
		"MediaStreams":[{"Index":0,"Type":"Video"},{"Index":2,"Type":"Audio","IsDefault":true}]}]}`, db.CompactID(item)))
	mustExec(t, e.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
		VALUES ($1, $1, 'film.mkv', $2, 'abc123', 'mkv', '[]')`, item, len(e.body))
	a.films = append(a.films, accessFilm{store: "s3", item: item, source: db.CompactID(item), body: e.body})

	ctx := context.Background()
	for _, film := range a.films {
		mustExec(t, e.pool, "UPDATE media_sources SET streams = $2 WHERE item_id = $1", film.item, accessStreams)
		source, found, err := e.apis[0].Items.Source(ctx, film.item, "")
		if err != nil || !found {
			t.Fatal(found, err)
		}
		assets, err := store.Open(source.AssetStoreURI)
		if err != nil {
			t.Fatal(err)
		}
		put := func(key string, body io.Reader) {
			t.Helper()
			if err := assets.Put(ctx, key, body); err != nil {
				t.Fatal(err)
			}
		}
		put(subtitles.Key(source, 3), subtitles.Track{VTT: vttOne, SRT: srtOne}.Encode())
		sum := sha256.Sum256(accessFont)
		fontHash := hex.EncodeToString(sum[:])
		put(subtitles.FontKey(fontHash), bytes.NewReader(accessFont))
		put(subtitles.FontsKey(source.ID, source.ETag), strings.NewReader(fmt.Sprintf(`{"6":%q}`, fontHash)))
		put("poster.jpg", bytes.NewReader(accessPicture))
		mustExec(t, e.pool, "UPDATE items SET primary_image_key = 'poster.jpg' WHERE id = $1", film.item)
		chapter := "chapters/" + film.item + "/dir/0.jpg"
		put(chapter, bytes.NewReader(accessPicture))
		mustExec(t, e.pool, `UPDATE items SET dto = dto || '{"Chapters":[{"StartPositionTicks":0,"Name":"Opening","ImageDateModified":"0001-01-01T00:00:00.0000000Z"}]}' WHERE id = $1`, film.item)
		mustExec(t, e.pool, "UPDATE libraries SET chapter_images = true WHERE id = (SELECT library_id FROM items WHERE id = $1)", film.item)
		mustExec(t, e.pool, `INSERT INTO chapter_images (source_id, library_id, etag, state, attempts, keys)
			SELECT $1, library_id, 'abc123', 'done', 1, $2 FROM items WHERE id = $1`, film.item, []string{chapter})
	}
	return a
}

// accHandler is one server of the matrix.
type accHandler struct {
	name  string
	base  string
	roles []string
}

func (h accHandler) serves(route Route) bool { return slices.Contains(h.roles, route.Role) }

func (a *accessSite) serve(t *testing.T, roles ...string) accHandler {
	t.Helper()
	pool, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Roles: roles, Log: slog.New(slog.DiscardHandler), SocketIdle: 700 * time.Millisecond}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	return accHandler{name: strings.Join(roles, "+"), base: server.URL, roles: roles}
}

// The seven accSpellings compat.ParseAuth reads, and -1 for no credential at all.
var accSpellings = []string{"Authorization MediaBrowser", "Authorization Emby", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token", "ApiKey", "api_key"}

type accReply struct {
	status int
	header http.Header
	body   []byte
	err    error
}

var noRedirect = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{DisableKeepAlives: true}}

// send asks one request, with the token in the spelling given (a negative spelling sends none).
func accSend(method, base, path string, token string, spelling int, body string, headers ...string) accReply {
	return accSendWith(noRedirect, method, base, path, token, spelling, body, headers...)
}

func accSendWith(client *http.Client, method, base, path string, token string, spelling int, body string, headers ...string) accReply {
	if spelling >= 5 && token != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + []string{"ApiKey", "api_key"}[spelling-5] + "=" + token
	}
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return accReply{err: err}
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		switch spelling {
		case 0:
			request.Header.Set("Authorization", `MediaBrowser Client="c", Device="d", DeviceId="access", Version="1", Token="`+token+`"`)
		case 1:
			request.Header.Set("Authorization", `Emby Client="c", Device="d", DeviceId="access", Version="1", Token="`+token+`"`)
		case 2:
			request.Header.Set("X-Emby-Authorization", `MediaBrowser Client="c", Device="d", DeviceId="access", Version="1", Token="`+token+`"`)
		case 3:
			request.Header.Set("X-Emby-Token", token)
		case 4:
			request.Header.Set("X-MediaBrowser-Token", token)
		}
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := client.Do(request)
	if err != nil {
		return accReply{err: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return accReply{status: response.StatusCode, header: response.Header, body: data, err: err}
}

var accWildcard = regexp.MustCompile(`\{(\w+)\}`)

// path fills a route's wildcards from the fixtures by name, and {file} by the route as well. A accWildcard the test has
// no value for is a failure naming the route: a new route cannot go unasked.
func (a *accessSite) path(route Route, film accessFilm, user string) (string, error) {
	var missing []string
	out := accWildcard.ReplaceAllStringFunc(route.Path, func(w string) string {
		name := w[1 : len(w)-1]
		switch name {
		case "itemId", "seriesId":
			return film.item
		case "sourceId":
			return film.source
		case "userId":
			return user
		case "preferenceId":
			return "home"
		case "index":
			return "3"
		case "startPositionTicks":
			return "0"
		case "segmentId":
			return "0.mp4"
		case "imageType":
			return "Primary"
		case "imageIndex":
			return "0"
		case "file":
			switch {
			case route.Path == "/Videos/{itemId}/{file}":
				return "stream"
			case strings.Contains(route.Path, "/Subtitles/"):
				return "Stream.vtt"
			}
		}
		missing = append(missing, w)
		return w
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("%s has a accWildcard the access test has no value for (%s): add one to accessSite.path, so that the route is asked", routeName(route), strings.Join(missing, ", "))
	}
	if route.Path == "/Videos/{itemId}/{file}" {
		out += "?Static=true&mediaSourceId=" + film.source
	}
	return out, nil
}

func accRequestBody(route Route) string {
	switch {
	case route.Method == "POST" && strings.HasSuffix(route.Path, "/Password"):
		// Not JSON: the sweep asks with a valid token many times over, and a wrong current password would count
		// toward the account's sign-in bounds.
		return "not json"
	case route.Path == "/Users/AuthenticateByName":
		return fmt.Sprintf(`{"Username":%q,"Pw":%q}`, accessUserName, accessUserPassword)
	case route.Method == "POST":
		return "{}"
	}
	return ""
}

// signIn is a client signing in by name from a device of its own, and the token it is given.
func accSignIn(t *testing.T, base, device string) (token, userID string) {
	t.Helper()
	r := accSend("POST", base, "/Users/AuthenticateByName", "", -1, fmt.Sprintf(`{"Username":%q,"Pw":%q}`, accessUserName, accessUserPassword),
		"Authorization", fmt.Sprintf(`MediaBrowser Client="c", Device="d", DeviceId=%q, Version="1"`, device))
	var signedIn struct {
		AccessToken string
		User        struct{ Id string }
	}
	if r.err != nil || r.status != 200 || json.Unmarshal(r.body, &signedIn) != nil || signedIn.AccessToken == "" {
		t.Fatalf("signing in as %s: %d %v %s", accessUserName, r.status, r.err, r.body)
	}
	return signedIn.AccessToken, signedIn.User.Id
}

// device inserts a signed-in device of the user, as a sign-in does.
func (a *accessSite) device(t *testing.T, user, deviceID string) string {
	t.Helper()
	token := uuid.NewString()
	mustExec(t, a.pool, `INSERT INTO devices (token, user_id, device_id, device_name, client, app_version) VALUES ($1, $2, $3, 'dev', 'cli', '1')`, token, user, deviceID)
	return token
}

// accepted says whether the accHandler takes the token: GET /Users/Me where it has the api role, the first ten bytes of
// the film where it has the stream alone.
func (a *accessSite) accepted(h accHandler, token string, spelling int) bool {
	if slices.Contains(h.roles, RoleAPI) {
		return accSend("GET", h.base, "/Users/Me", token, spelling, "").status == 200
	}
	r := accSend("GET", h.base, a.rangePath(a.films[0]), token, spelling, "", "Range", "bytes=0-9")
	return r.status == 206
}

func (a *accessSite) rangePath(film accessFilm) string {
	return "/Videos/" + film.source + "/stream?Static=true&mediaSourceId=" + film.source
}

// A accCause is a way a token stops being accepted, after it was.
type accCause struct {
	name string
	// make returns a signed-in device's token, and what ends it, done through via.
	make func(t *testing.T, a *accessSite, via accHandler, n int) (token string, end func())
}

var accCauses = []accCause{
	{"signed out through POST /Sessions/Logout", func(t *testing.T, a *accessSite, via accHandler, n int) (string, func()) {
		token := a.device(t, a.user, fmt.Sprintf("so-%d", n))
		return token, func() {
			if r := accSend("POST", via.base, "/Sessions/Logout", token, 3, ""); r.status != 204 {
				t.Fatalf("logout of %s: %d %s", token, r.status, r.body)
			}
		}
	}},
	{"its user was deleted from the users table", func(t *testing.T, a *accessSite, via accHandler, n int) (string, func()) {
		id := uuid.NewString()
		mustExec(t, a.pool, `INSERT INTO users (id, name, password_hash) VALUES ($1, $2, 'x')`, id, fmt.Sprintf("leaver-%d", n))
		return a.device(t, id, "d"), func() { mustExec(t, a.pool, "DELETE FROM users WHERE id = $1", id) }
	}},
	{"the same user and DeviceId signed in again", func(t *testing.T, a *accessSite, via accHandler, n int) (string, func()) {
		device := fmt.Sprintf("rep-%d", n)
		token := a.device(t, a.user, device)
		return token, func() {
			if again, _ := accSignIn(t, via.base, device); again == token {
				t.Fatal("signing in again gave the same token")
			}
		}
	}},
}

// invalidated is a token that h accepted (a 200 from GET /Users/Me, or a range of the film from the stream alone) and
// does not now, by the cause, which is done through via.
func (a *accessSite) invalidated(t *testing.T, c accCause, h, via accHandler, n int) string {
	t.Helper()
	token, undo := c.make(t, a, via, n)
	a.mustAccept(t, h, token, 3)
	undo()
	return token
}

func (a *accessSite) mustAccept(t *testing.T, h accHandler, token string, spelling int) {
	t.Helper()
	if !a.accepted(h, token, spelling) {
		t.Fatalf("accHandler %s does not accept a token it was just given (spelling %d)", h.name, spelling)
	}
}

// accUniform keeps the shape of every 401 so that all are the same, whatever route, accCause or spelling.
type accUniform struct{ seen map[string][]string }

func accShape(r accReply) string {
	var names []string
	for name := range r.header {
		if name != "Date" {
			names = append(names, name+": "+strings.Join(r.header[name], ", "))
		}
	}
	sort.Strings(names)
	return fmt.Sprintf("%d\n%s\n\n%s", r.status, strings.Join(names, "\n"), r.body)
}

// judge holds one answer to what the route declares. what names the request for the failure.
func (a *accessSite) judge(u *accUniform, route Route, what string, r accReply, head bool) {
	name := routeName(route) + " " + what
	if r.err != nil {
		a.fail("%s: %v", name, r.err)
		return
	}
	if accOpen(route) {
		if slices.Contains(openButAsks, routeName(route)) {
			if r.status == 401 || r.status == 403 || r.status >= 500 {
				a.fail("%s is open and answered %d %.60s", name, r.status, r.body)
			}
			return
		}
		if r.status/100 != 2 {
			a.fail("%s is open and answered %d %.60s", name, r.status, r.body)
		}
		return
	}
	if r.status != 401 {
		a.fail("%s answered %d %.60q, want 401", name, r.status, r.body)
		return
	}
	if !head {
		// JS-183: jellyscale's own API answers the sign-in refusal as JSON (contract_integration_test.go); Jellyfin's routes keep the text.
		if strings.HasPrefix(route.Path, "/jellyscale/v1/") {
			if !isSignInRefusal(r.body) {
				a.fail("%s: the body is %q, want the sign-in refusal as JSON", name, r.body)
			}
		} else if strings.TrimSpace(string(r.body)) != "unauthorized" || len(r.body) >= 64 {
			a.fail("%s: the body is %q, want unauthorized, under 64 bytes", name, r.body)
		}
		u.seen[accShape(r)] = append(u.seen[accShape(r)], name)
	}
	for _, leak := range []string{"Content-Range", "Content-Disposition", "ETag", "Location"} {
		if r.header.Get(leak) != "" {
			a.fail("%s: a 401 carries %s: %s", name, leak, r.header.Get(leak))
		}
	}
}

func (u *accUniform) check(a *accessSite, handlerName string) {
	if len(u.seen) > 1 {
		var kinds []string
		for s, who := range u.seen {
			kinds = append(kinds, fmt.Sprintf("%d answers like %q, the first %s", len(who), strings.ReplaceAll(s, "\n", "|"), who[0]))
		}
		sort.Strings(kinds)
		a.fail("%s: the 401 is not the same for every route and accCause: %s", handlerName, strings.Join(kinds, "; "))
	}
}

// sameAs holds HEAD to the GET: the same status and headers.
func (a *accessSite) sameAs(route Route, what string, get, head accReply) {
	name := routeName(route) + " " + what
	if get.err != nil || head.err != nil {
		a.fail("%s: %v %v", name, get.err, head.err)
		return
	}
	if get.status != head.status {
		a.fail("%s: GET answered %d and HEAD %d", name, get.status, head.status)
		return
	}
	for key, want := range get.header {
		if key == "Date" || (get.status/100 == 2 && (key == "Content-Length" || key == "Transfer-Encoding")) {
			continue
		}
		if !slices.Equal(want, head.header[key]) {
			a.fail("%s: header %s is %v for GET and %v for HEAD", name, key, want, head.header[key])
		}
	}
	for key := range head.header {
		if _, ok := get.header[key]; !ok && key != "Date" {
			a.fail("%s: HEAD has header %s that GET has not", name, key)
		}
	}
}

// matrix asks each of the routes the accHandler serves, for one film, every way the criteria name.
func (a *accessSite) matrix(t *testing.T, h accHandler, via accHandler, film accessFilm, user string) {
	t.Helper()
	u := &accUniform{seen: map[string][]string{}}
	n := 0
	for i, route := range (&Server{}).Routes() {
		if !h.serves(route) {
			continue
		}
		path, err := a.path(route, film, user)
		if err != nil {
			a.fail("%v", err)
			continue
		}
		a.asked[h.name+" "+routeName(route)]++
		body := accRequestBody(route)
		viewer := !accOpen(route)
		query := ""
		if before, after, found := strings.Cut(path, "?"); found {
			path, query = before, "?"+after
		}
		upper := strings.ToUpper(path) + "/" + query
		path += query

		// No token, and an unknown token in each spelling; the plain and the upper-case path, and HEAD of a GET.
		ask := func(what, method, p, token string, spelling int, headers ...string) accReply {
			r := accSend(method, h.base, p, token, spelling, body, headers...)
			a.judge(u, route, what, r, method == "HEAD")
			return r
		}
		plain := ask("with no token", route.Method, path, "", -1)
		ask("with no token, path in upper case with a trailing slash", route.Method, upper, "", -1)
		for s := range accSpellings {
			ask("with an unknown token, "+accSpellings[s], route.Method, path, "unknown-"+uuid.NewString(), s)
		}
		if route.Method == "GET" {
			head := accSend("HEAD", h.base, path, "", -1, "")
			a.judge(u, route, "HEAD with no token", head, true)
			a.sameAs(route, "with no token", plain, head)
			unknown := "unknown-" + uuid.NewString()
			a.sameAs(route, "with an unknown token", accSend("GET", h.base, path, unknown, 3, ""), accSend("HEAD", h.base, path, unknown, 3, ""))
			a.judge(u, route, "HEAD with an unknown token", accSend("HEAD", h.base, path, unknown, 3, ""), true)
			ask("with no token, path in upper case with a trailing slash, HEAD", "HEAD", upper, "", -1)
		}
		if route.Path == "/socket" {
			handshake := []string{"Connection", "Upgrade", "Upgrade", "websocket", "Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="}
			r := accSend("GET", h.base, path, "", -1, "", handshake...)
			a.judge(u, route, "as a WebSocket handshake with no token", r, false)
			if r.status == 101 {
				a.fail("%s: a handshake with no token was upgraded", routeName(route))
			}
			r = accSend("GET", h.base, path, "unknown-"+uuid.NewString(), 3, "", handshake...)
			a.judge(u, route, "as a WebSocket handshake with an unknown token", r, false)
		}

		// A token that was accepted and is not now: the first request it makes, in a spelling that goes round.
		for c, why := range accCauses {
			n++
			token := a.invalidated(t, why, h, via, n)
			ask("with a token that was accepted until "+why.name+", "+accSpellings[(i+c)%len(accSpellings)], route.Method, path, token, (i+c)%len(accSpellings))
		}

		// A valid token, each spelling, as the user's own device would ask, with the user's own id in the path.
		for s := range accSpellings {
			token, p := a.device(t, user, fmt.Sprintf("valid-%s-%d-%d", h.name, i, s)), path
			if route.Path == "/Sessions/Logout" {
				// The token is signed in for this request, and the request signs it out.
				if r := accSend("POST", h.base, p, token, s, ""); r.status != 204 {
					a.fail("%s with a valid token, %s: %d %.60q, want 204", routeName(route), accSpellings[s], r.status, r.body)
				}
				continue
			}
			r := accSend(route.Method, h.base, p, token, s, body)
			switch {
			case r.err != nil:
				a.fail("%s with a valid token, %s: %v", routeName(route), accSpellings[s], r.err)
			case r.status == 401 || r.status == 403 && !viewer:
				a.fail("%s with a valid token, %s: answered %d %.60q", routeName(route), accSpellings[s], r.status, r.body)
			case !viewer && r.status/100 != 2 && !slices.Contains(openButAsks, routeName(route)):
				a.fail("%s is open and answered %d to a valid token, %s", routeName(route), r.status, accSpellings[s])
			}
		}
	}
	u.check(a, h.name)
}

// media asks for what the fixture holds, so that a 401 is never a route that is broken for everybody: the first ten
// bytes of each store's film in every spelling, a subtitle, a font, a download, and the pictures, which anyone gets.
func (a *accessSite) media(t *testing.T, h accHandler, token string) {
	t.Helper()
	for _, film := range a.films {
		if !slices.Contains(h.roles, RoleStream) {
			return
		}
		for s := range accSpellings {
			r := accSend("GET", h.base, a.rangePath(film), token, s, "", "Range", "bytes=0-9")
			if r.err != nil || r.status != 206 || !bytes.Equal(r.body, film.body[:10]) {
				a.fail("%s: the first ten bytes of the %s film with a token in the spelling %s: %d %q %v, want 206 and the file's own", h.name, film.store, accSpellings[s], r.status, r.body, r.err)
			}
		}
		for _, p := range []string{
			"/Videos/" + film.item + "/" + film.source + "/Subtitles/3/0/Stream.vtt",
			"/Videos/" + film.item + "/" + film.source + "/Attachments/6",
			"/Items/" + film.item + "/Download",
		} {
			if r := accSend("GET", h.base, p, token, 3, ""); r.err != nil || r.status/100 != 2 {
				a.fail("%s: %s with a valid token (%s film): %d %.60q %v", h.name, p, film.store, r.status, r.body, r.err)
			}
			// What the token buys is never given without it.
			if r := accSend("GET", h.base, p, "", -1, ""); r.status != 401 || len(r.body) >= 64 {
				a.fail("%s: %s with no token (%s film): %d %.60q, want the 401", h.name, p, film.store, r.status, r.body)
			}
		}
		// The pictures are open: for each store's film, the poster and a chapter's, with no token at all.
		for _, p := range []string{"/Items/" + film.item + "/Images/Primary", "/Items/" + film.item + "/Images/Primary/0",
			"/Items/" + film.item + "/Images/Chapter", "/Items/" + film.item + "/Images/Chapter/0"} {
			if r := accSend("GET", h.base, p, "", -1, ""); r.err != nil || r.status != 200 || !bytes.Equal(r.body, accessPicture) {
				a.fail("%s: %s (%s film) with no token: %d %.30q %v, want 200 and the picture", h.name, p, film.store, r.status, r.body, r.err)
			}
		}
	}
}

// neighbours: invalidating one token touches no other. Signing out or replacing one device's token leaves the same
// user's other device accepted, and deleting one user leaves the others accepted.
func (a *accessSite) neighbours(t *testing.T, h accHandler, via accHandler) {
	t.Helper()
	for c, why := range accCauses {
		other := a.device(t, a.user, fmt.Sprintf("other-%s-%d", h.name, c))
		a.mustAccept(t, h, other, 3)
		gone := a.invalidated(t, why, h, via, 9000+c*10+len(h.name))
		if a.accepted(h, gone, 3) {
			a.fail("%s: a token that was accepted until %s is accepted", h.name, why.name)
		}
		if !a.accepted(h, other, 3) {
			a.fail("%s: invalidating a token by %s took the same user's other device's token with it", h.name, why.name)
		}
		if !a.accepted(h, a.token, 0) {
			a.fail("%s: invalidating a token by %s took the run's own token with it", h.name, why.name)
		}
	}
}

// outside: nothing but Routes() and the probes is served: every method a route does not declare, every path no
// route has, and the web client's paths with no web directory, answer the catch-all's 404, with or without a token.
// accPattern is a route's path as a pattern over lower-case request paths.
func accPattern(path string) *regexp.Regexp {
	parts := accWildcard.Split(path, -1)
	for i := range parts {
		parts[i] = regexp.QuoteMeta(strings.ToLower(parts[i]))
	}
	return regexp.MustCompile("^" + strings.Join(parts, "[^/]+") + "$")
}

func (a *accessSite) outside(t *testing.T, h accHandler) {
	t.Helper()
	routes := (&Server{}).Routes()
	for _, route := range routes {
		path, err := a.path(route, a.films[0], a.user)
		if err != nil {
			continue
		}
		path, _, _ = strings.Cut(path, "?")
		for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
			// A path some route of the handler's answers by a wildcard (GET /Users/Configuration is {userId}) is that route's.
			if method == "GET" && !h.serves(route) || slices.ContainsFunc(routes, func(other Route) bool {
				return h.serves(other) && other.Method == method && accPattern(other.Path).MatchString(strings.ToLower(path))
			}) {
				continue
			}
			for _, token := range []string{"", a.token} {
				r := accSend(method, h.base, path, token, 3, "")
				if r.err != nil || r.status != 404 || string(r.body) != "404 page not found\n" {
					a.fail("%s: %s %s (token %v) answered %d %.40q, want the catch-all's 404", h.name, method, path, token != "", r.status, r.body)
				}
			}
		}
	}
	for _, p := range []string{"/", "/web/", "/web/index.html", "/Items/x/y/z/w", "/System/Configuration", "/System/Info/Private", "/metrics", "/Videos", "/Items/" + a.item + "/Images", "/healthz/x"} {
		for _, token := range []string{"", a.token} {
			if r := accSend("GET", h.base, p, token, 3, ""); r.err != nil || r.status != 404 {
				a.fail("%s: GET %s (token %v) answered %d, want the catch-all's 404", h.name, p, token != "", r.status)
			}
		}
	}
	for _, token := range []string{"", a.token} {
		// /web alone is redirected to /web/ before the mux, and there is no web directory to serve.
		if r := accSend("GET", h.base, "/web", token, 3, ""); r.status != 302 || r.header.Get("Location") != "/web/" {
			a.fail("%s: GET /web answered %d to %q, want a redirect to /web/", h.name, r.status, r.header.Get("Location"))
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		for _, p := range []string{"/healthz", "/readyz"} {
			if r := accSend(method, h.base, p, a.token, 3, ""); r.status != 404 {
				a.fail("%s: %s %s answered %d, want the catch-all's 404", h.name, method, p, r.status)
			}
		}
	}
	if r := accSend("GET", h.base, "/healthz", "", -1, ""); r.status != 204 {
		a.fail("%s: GET /healthz answered %d, want 204", h.name, r.status)
	}
	if r := accSend("GET", h.base, "/readyz", "", -1, ""); r.status != 200 {
		a.fail("%s: GET /readyz answered %d, want 200", h.name, r.status)
	}
}

// accOpen is whether the open list of access_test.go, which this test holds to and not the route's own Access, has the
// route: a route that is declared open by mistake (the film's bytes, say) is then asked as the viewer route it must be,
// and answers 200 where the test wants the 401.
func accOpen(route Route) bool { return slices.Contains(openList, routeName(route)) }

// The API and the stream together are one process, and each alone is one: the same routes behave the same.
func TestAccess(t *testing.T) {
	a := newAccessSite(t)
	routes := (&Server{}).Routes()
	if len(routes) < 88 {
		t.Fatalf("%d routes, want at least the 88 there were", len(routes))
	}
	both, apiOnly, streamOnly := a.serve(t, RoleAPI, RoleStream), a.serve(t, RoleAPI), a.serve(t, RoleStream)
	for _, h := range []accHandler{both, apiOnly, streamOnly} {
		for _, film := range a.films {
			a.matrix(t, h, both, film, a.user)
		}
		a.media(t, h, a.token)
		a.neighbours(t, h, both)
		a.outside(t, h)
		a.scope(t, h)
		a.administrators(t, h)
		a.report(t)
	}
	// Every route was asked, on every accHandler that serves it, and a route that no accHandler serves is a failure.
	for _, route := range routes {
		// JS-183: an open route says why (Route.Why, docs/access.md); nothing else reads it.
		if route.Access == AccessAnyone && route.Why == "" {
			t.Errorf("%s is open and says no why", routeName(route))
		}
		for _, h := range []accHandler{both, apiOnly, streamOnly} {
			if h.serves(route) && a.asked[h.name+" "+routeName(route)] == 0 {
				t.Errorf("%s was not asked on the accHandler with roles %s", routeName(route), h.name)
			}
		}
		if !slices.Contains([]string{RoleAPI, RoleStream}, route.Role) {
			t.Errorf("%s has role %q, which no accHandler of the test serves", routeName(route), route.Role)
		}
	}
	t.Logf("%d routes asked on %d handlers", len(routes), 3)
}

// A route that is open answers 2xx to every request the viewer routes answer 401: the token that was accepted until
// it was signed out, its user was deleted or its device signed in again, no token, unknown, HEAD, the upper-case path,
// and the sign-in itself, which hands out a token that then works. The matrix of TestAccess asks them all of every
// route; this is the part about the sign-in, by name.
func TestSigningInHandsOutATokenThatWorksOnEveryHandler(t *testing.T) {
	a := newAccessSite(t)
	both, streamOnly := a.serve(t, RoleAPI, RoleStream), a.serve(t, RoleStream)
	token, user := accSignIn(t, both.base, "fresh-device")
	for _, h := range []accHandler{both, streamOnly} {
		for s := range accSpellings {
			if !a.accepted(h, token, s) {
				t.Errorf("a token from POST /Users/AuthenticateByName is not accepted by %s in the spelling %s", h.name, accSpellings[s])
			}
		}
	}
	if r := accSend("GET", both.base, "/Users/"+user, token, 3, ""); r.status == 401 {
		t.Errorf("the caller's own id in the path is refused: %d", r.status)
	}
}
