//go:build integration

package api

// JS-183 review, class 1. Every answer a v1 operation can give is declared by the contract: its status, and for each status the
// reasons it can carry, including a reason a 200 body carries beside Jellyfin's ErrorCode. The requests here are not picked by
// hand. They come from the served contract: each operation's method and path, the scope names every v1 route reads (userId and
// controllableByUserId, in the query and in a PlaybackInfo body), the body fields it declares (a valid value and a wrong one of
// each), the token forms, the content types, and the states a request can meet: an account whose row is gone at its write, a
// database that fails, a title that is a series, a title that is unknown, an id that is malformed, and a film that needs a
// transcode no worker can make. Each request varies one fact of a valid one, and then the token with the scope, and the scope with
// each body. Every answer is judged against what the contract declares for its route. The server's log line for an answer it
// refused to send (docs/running.md) is judged too.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/testenv"
)

// matrixLogLine is the line the server writes for an answer its contract does not declare (docs/running.md names it).
const matrixLogLine = "undeclared answer"

// matrixOp is one path and method of the served contract, with the statuses its responses declare and the reasons for each.
type matrixOp struct {
	method, path, state string
	statuses            map[string]map[string]bool
	hasBody             bool
	bodyFields          map[string]string // the body's declared fields and their JSON types
}

func (op matrixOp) key() string { return op.method + " " + op.path }

// matrixOps reads every operation of the served document, in order, with what each response declares.
func matrixOps(t *testing.T, document map[string]any) []matrixOp {
	t.Helper()
	paths, _ := document["paths"].(map[string]any)
	var ops []matrixOp
	for path, item := range paths {
		if !strings.HasPrefix(path, "/jellyscale/v1/") {
			continue
		}
		methods, _ := item.(map[string]any)
		for method, value := range methods {
			if !slices.Contains(httpMethods, method) {
				continue
			}
			fields, _ := value.(map[string]any)
			op := matrixOp{method: strings.ToUpper(method), path: path, state: fmt.Sprint(fields["x-state"]), statuses: map[string]map[string]bool{}}
			responses, _ := fields["responses"].(map[string]any)
			for status := range responses {
				op.statuses[status] = map[string]bool{}
				for _, reason := range matrixReasons(t, document, path, method, status) {
					op.statuses[status][reason] = true
				}
			}
			if _, ok := fields["requestBody"]; ok {
				op.hasBody = true
				op.bodyFields = matrixBodyFields(t, document, path, method)
			}
			ops = append(ops, op)
		}
	}
	slices.SortFunc(ops, func(a, b matrixOp) int { return strings.Compare(a.key(), b.key()) })
	return ops
}

// matrixReasons is the x-reasons a response declares, after its $refs.
func matrixReasons(t *testing.T, document map[string]any, path, method, status string) []string {
	t.Helper()
	response := nodeAt(t, document, settle(t, document, "/paths/"+tokenEscape.Replace(path)+"/"+method+"/responses/"+tokenEscape.Replace(status)))
	listed, _ := response["x-reasons"].([]any)
	var reasons []string
	for _, entry := range listed {
		if code, ok := entry.(string); ok {
			reasons = append(reasons, code)
		}
	}
	return reasons
}

// matrixBodyFields is the JSON type of each field the request body schema declares.
func matrixBodyFields(t *testing.T, document map[string]any, path, method string) map[string]string {
	t.Helper()
	schema := nodeAt(t, document, settle(t, document, "/paths/"+tokenEscape.Replace(path)+"/"+method+"/requestBody/content/"+tokenEscape.Replace("application/json")+"/schema"))
	properties, _ := schema["properties"].(map[string]any)
	fields := map[string]string{}
	for name, value := range properties {
		property, _ := value.(map[string]any)
		kind, _ := property["type"].(string)
		fields[name] = kind
	}
	return fields
}

// matrixWrongValue is a JSON value of the wrong type for a field of the given type.
func matrixWrongValue(kind string) string {
	switch kind {
	case "boolean":
		return `"yes"`
	case "array", "object":
		return `"x"`
	case "string":
		return "5"
	default:
		return "null"
	}
}

// matrixValidValue is a JSON value of the declared type.
func matrixValidValue(kind string) string {
	switch kind {
	case "boolean":
		return "true"
	case "array":
		return "[]"
	case "integer":
		return "1"
	case "object":
		return "{}"
	default:
		return `"x"`
	}
}

// matrixMethods are the methods a request may name, the ones HTTP defines.
var matrixMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}

// matrixVariant is one request and the operation and state it is asked in.
type matrixVariant struct {
	name, state, base, token, method, path, query, contentType string
	body                                                       []byte
}

// matrixWorld is what the requests are built from: the accounts, the titles and the servers of the states.
type matrixWorld struct {
	base, failure                string
	caller, callerToken          string
	other, vanisher, vanisherTok string
	item, series, unknown        string
}

func (w matrixWorld) scopeValues() []string {
	return []string{w.caller, db.DashedID(w.caller), w.other, "00000000-0000-0000-0000-000000000000", "not-an-id", ""}
}

// matrixBodies are the bodies an operation may be sent: absent, empty, not an object, an object with a field it does not have, each
// declared field with a valid value and with one of the wrong type, a duplicated field, a body over both limits, the scope fields of
// a body (the caller's own and another's), and a media source the item does not have.
func matrixBodies(op matrixOp, world matrixWorld) map[string][]byte {
	bodies := map[string][]byte{
		"absent": nil, "empty": []byte(""), "null": []byte("null"), "array": []byte("[]"), "number": []byte("42"),
		"string": []byte(`"text"`), "truncated": []byte("{"), "unknown-field": []byte(`{"NoSuchField":1}`),
		"duplicate-field":      []byte(`{"UserId":"x","UserId":"y"}`),
		"over-limits":          []byte(`{"KeepAudioLanguages":["` + strings.Repeat("a", 70000) + `"]}`),
		"user-id-own":          []byte(`{"UserId":"` + world.caller + `"}`),
		"user-id-other":        []byte(`{"UserId":"` + world.other + `"}`),
		"user-id-invalid":      []byte(`{"UserId":"not-an-id"}`),
		"media-source-unknown": []byte(`{"MediaSourceId":"not-a-source"}`),
	}
	for name, kind := range op.bodyFields {
		bodies["valid-"+name] = []byte(`{"` + name + `":` + matrixValidValue(kind) + `}`)
		bodies["wrong-type-"+name] = []byte(`{"` + name + `":` + matrixWrongValue(kind) + `}`)
	}
	return bodies
}

// matrixVariants are the requests one operation is asked in one state, from a valid base request. A state may give its own base
// body, as the no-worker state does: a body whose film needs a transcode.
func matrixVariants(w matrixWorld, op matrixOp, state, base, token, path string, stateBody []byte) []matrixVariant {
	baseBody := []byte(nil)
	contentType := ""
	if op.hasBody {
		baseBody, contentType = []byte("{}"), "application/json"
	}
	if stateBody != nil {
		baseBody = stateBody
	}
	seed := matrixVariant{state: state, base: base, token: token, method: op.method, path: path, body: baseBody, contentType: contentType}
	var out []matrixVariant
	add := func(name string, edit func(v *matrixVariant)) {
		v := seed
		v.name = name
		edit(&v)
		out = append(out, v)
	}
	add("base", func(*matrixVariant) {})
	add("no token", func(v *matrixVariant) { v.token = "" })
	add("unknown token", func(v *matrixVariant) { v.token = "not-a-token" })
	for _, method := range matrixMethods {
		if method != op.method {
			add("method "+method, func(v *matrixVariant) { v.method = method })
		}
	}
	add("trailing slash", func(v *matrixVariant) { v.path += "/" })
	add("upper-case path", func(v *matrixVariant) { v.path = strings.ToUpper(v.path) })
	add("extra segment", func(v *matrixVariant) { v.path += "/extra" })
	add("parent path", func(v *matrixVariant) { v.path = v.path[:strings.LastIndex(v.path, "/")] })
	add("no content type", func(v *matrixVariant) { v.contentType = "" })
	add("text content type", func(v *matrixVariant) { v.contentType = "text/plain" })
	for _, key := range []string{"userId", "controllableByUserId"} {
		for _, value := range w.scopeValues() {
			add("query "+key+"="+value, func(v *matrixVariant) { v.query = url.Values{key: {value}}.Encode() })
		}
	}
	for _, token := range []string{"", "not-a-token"} {
		for _, value := range w.scopeValues() {
			add("token "+token+" with query "+value, func(v *matrixVariant) {
				v.token = token
				v.query = url.Values{"userId": {value}}.Encode()
			})
		}
	}
	if !op.hasBody {
		return out
	}
	bodies := matrixBodies(op, w)
	names := make([]string, 0, len(bodies))
	for name := range bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		add("body "+name, func(v *matrixVariant) { v.body = bodies[name] })
		if name == "absent" {
			continue
		}
		for _, value := range w.scopeValues() {
			add("body "+name+" with query "+value, func(v *matrixVariant) {
				v.body = bodies[name]
				v.query = url.Values{"userId": {value}}.Encode()
			})
		}
	}
	return out
}

// matrixExpect is what the contract declares for a request: the statuses and reasons of the operation with its method and path.
// A method no operation of the path takes is the dispatcher's answer, which the contract declares on the path's operations: 405 on
// a served path, or not-served (404) on a path whose operations are all declared. A path no operation has is not-found.
func matrixExpect(ops []matrixOp, method, path string) (map[string]map[string]bool, string) {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	lowered := strings.ToLower(path)
	var onPath []matrixOp
	for _, op := range ops {
		if !matchV1Path(op.path, lowered) {
			continue
		}
		if op.method == method {
			return op.statuses, op.key()
		}
		onPath = append(onPath, op)
	}
	if len(onPath) == 0 {
		return map[string]map[string]bool{"404": {"not-found": true}}, "no operation"
	}
	want := "404"
	for _, op := range onPath {
		if op.state != "declared" {
			want = "405"
		}
	}
	reasons := map[string]bool{}
	for _, op := range onPath {
		for reason := range op.statuses[want] {
			reasons[reason] = true
		}
	}
	return map[string]map[string]bool{want: reasons}, method + " " + onPath[0].path
}

// matrixJudge lists what one answer shows the contract does not declare. A problem is worded without the request's own values, so
// the same problem found by many requests counts once.
func matrixJudge(expected map[string]map[string]bool, key string, response *http.Response, raw []byte, reasons map[string]reasonInfo) []string {
	status := strconv.Itoa(response.StatusCode)
	declared, ok := expected[status]
	if !ok {
		return []string{fmt.Sprintf("status %s is not declared for %s", status, key)}
	}
	if response.Request.Method == http.MethodHead {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		if response.StatusCode >= 400 {
			return []string{fmt.Sprintf("status %s of %s has a body that is not a JSON object", status, key)}
		}
		return nil
	}
	var problems []string
	if response.StatusCode >= 400 {
		if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
			problems = append(problems, fmt.Sprintf("status %s of %s is not answered as JSON", status, key))
		}
		for name := range fields {
			if name != "Reason" && name != "Action" && name != "RetryAfterSeconds" {
				problems = append(problems, fmt.Sprintf("status %s of %s carries the key %s", status, key, name))
			}
		}
	}
	rawReason, hasReason := fields["Reason"]
	if !hasReason {
		if response.StatusCode >= 400 {
			problems = append(problems, fmt.Sprintf("status %s of %s has no Reason", status, key))
		}
		return problems
	}
	var reason, action string
	if err := json.Unmarshal(rawReason, &reason); err != nil {
		return append(problems, fmt.Sprintf("the Reason of %s is not a string", key))
	}
	_ = json.Unmarshal(fields["Action"], &action)
	if !declared[reason] {
		problems = append(problems, fmt.Sprintf("reason %s is not declared for status %s of %s", reason, status, key))
	}
	if listed := reasons[reason]; listed.Kind != "refusal" {
		problems = append(problems, fmt.Sprintf("reason %s is carried by status %s of %s but is not a refusal", reason, status, key))
	} else if listed.Action != action {
		problems = append(problems, fmt.Sprintf("reason %s is answered with action %s, the contract gives %s", reason, action, listed.Action))
	}
	return problems
}

// matrixFinding is one problem, how many requests showed it, and one of them.
type matrixFinding struct {
	count   int
	example string
}

// matrixRecord judges one answer into the findings.
func matrixRecord(findings map[string]*matrixFinding, reasons map[string]reasonInfo, ops []matrixOp, v matrixVariant, response *http.Response, raw []byte) {
	expected, key := matrixExpect(ops, v.method, v.path)
	for _, problem := range matrixJudge(expected, key, response, raw, reasons) {
		finding := findings[problem]
		if finding == nil {
			finding = &matrixFinding{example: fmt.Sprintf("[%s] %s (%s %s) answered %d %.100q", v.state, v.name, v.method, v.path, response.StatusCode, raw)}
			findings[problem] = finding
		}
		finding.count++
	}
}

// matrixReport fails the test with every distinct problem, most frequent first, and one example of each.
func matrixReport(t *testing.T, findings map[string]*matrixFinding, answers int) {
	t.Helper()
	if len(findings) == 0 {
		t.Logf("%d answers were asked, and every one is declared", answers)
		return
	}
	problems := make([]string, 0, len(findings))
	for problem := range findings {
		problems = append(problems, problem)
	}
	sort.Slice(problems, func(i, j int) bool {
		if findings[problems[i]].count != findings[problems[j]].count {
			return findings[problems[i]].count > findings[problems[j]].count
		}
		return problems[i] < problems[j]
	})
	var report strings.Builder
	for _, problem := range problems {
		fmt.Fprintf(&report, "  %5d  %s\n         e.g. %s\n", findings[problem].count, problem, findings[problem].example)
	}
	t.Errorf("%d answers were asked; %d distinct answers the contract does not declare:\n%s", answers, len(problems), report.String())
}

// matrixLogs keeps the lines the server writes during the matrix, from its request goroutines.
type matrixLogs struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *matrixLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *matrixLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

// matrixSeries makes a series: a title that exists with no file of its own.
func matrixSeries(t *testing.T, kit *env) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, kit.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		VALUES ($1, (SELECT id FROM libraries ORDER BY name LIMIT 1), 'Series', 'Show', 'show', $2, 0, '{"Name":"Show","Type":"Series"}')`,
		id, "series-"+id)
	return id
}

// matrixSuppressWrites makes every UPDATE of one account match no row, as if the account had gone between its sign-in check and
// its write. The trigger is dropped when the test ends, before the environment's own cleanup.
func matrixSuppressWrites(t *testing.T, kit *env, id string) {
	t.Helper()
	name := "js183_suppress_" + strings.ReplaceAll(db.CompactID(id), "-", "")
	t.Cleanup(func() {
		kit.pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+name+" ON users") //nolint:errcheck
		kit.pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+name+"()")       //nolint:errcheck
	})
	mustExec(t, kit.pool, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF OLD.id = '`+id+`'::uuid THEN RETURN NULL; END IF; RETURN NEW; END $$`)
	mustExec(t, kit.pool, `CREATE TRIGGER `+name+` BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION `+name+`()`)
}

// matrixFailureServer is a server whose database pool is closed: every query fails at once, which is the failure answer. Its log
// is the matrix's, so an undeclared failure answer is judged too.
func matrixFailureServer(t *testing.T, log *slog.Logger) string {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	server := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Name: "jellyscale", ServerID: "srv",
		Roles: []string{RoleAPI}, Log: log}
	failure := httptest.NewServer(server.Handler())
	t.Cleanup(failure.Close)
	return failure.URL
}

// matrixSend asks one request and reads its answer.
func matrixSend(t *testing.T, v matrixVariant) (*http.Response, []byte) {
	t.Helper()
	target := v.base + v.path
	if v.query != "" {
		target += "?" + v.query
	}
	request, err := http.NewRequest(v.method, target, bytes.NewReader(v.body))
	if err != nil {
		t.Fatal(err)
	}
	if v.token != "" {
		request.Header.Set("X-Emby-Token", v.token)
	}
	if v.contentType != "" {
		request.Header.Set("Content-Type", v.contentType)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s (%s): %v", v.method, v.path, v.name, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, raw
}

// TestEveryAnswerOfEveryV1OperationIsDeclaredByTheContract asks every request of matrixVariants in every state each operation can
// meet, and reports every answer whose status or reason the contract does not declare for its route, and every undeclared answer
// the server logged. The base request of each operation is sent first, so a wrong base fails here, not as a problem.
func TestEveryAnswerOfEveryV1OperationIsDeclaredByTheContract(t *testing.T) {
	kit := newEnv(t)
	document := servedContract(t, kit)
	ops := matrixOps(t, document)
	reasons := reasonsOf(t, document)
	vanisher, vanisherToken := kit.addUser(t, "vanisher")
	matrixSuppressWrites(t, kit, vanisher)
	series := matrixSeries(t, kit)
	var logs matrixLogs
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	previous := kit.apis[0].Log
	kit.apis[0].Log = logger
	t.Cleanup(func() { kit.apis[0].Log = previous })
	world := matrixWorld{
		base: kit.servers[0].URL, failure: matrixFailureServer(t, logger),
		caller: db.CompactID(kit.user), callerToken: kit.token, other: db.CompactID(kit.other),
		vanisher: db.CompactID(vanisher), vanisherTok: vanisherToken,
		item: db.CompactID(kit.item), series: db.CompactID(series), unknown: "00000000000000000000000000000000",
	}
	findings := map[string]*matrixFinding{}
	answers := 0
	for _, op := range ops {
		type state struct{ name, base, token, path string }
		states := []state{
			{"normal", world.base, world.callerToken, strings.ReplaceAll(op.path, "{itemId}", world.item)},
			{"account gone at its write", world.base, world.vanisherTok, strings.ReplaceAll(op.path, "{itemId}", world.item)},
			{"database failure", world.failure, world.callerToken, strings.ReplaceAll(op.path, "{itemId}", world.item)},
		}
		if strings.Contains(op.path, "{itemId}") {
			states = append(states,
				state{"series", world.base, world.callerToken, strings.ReplaceAll(op.path, "{itemId}", world.series)},
				state{"unknown title", world.base, world.callerToken, strings.ReplaceAll(op.path, "{itemId}", world.unknown)},
				state{"malformed title", world.base, world.callerToken, strings.ReplaceAll(op.path, "{itemId}", "not-an-id")})
		}
		for _, st := range states {
			for _, v := range matrixVariants(world, op, st.name, st.base, st.token, st.path, nil) {
				response, raw := matrixSend(t, v)
				if v.name == "base" && st.name == "normal" {
					want := "200"
					if op.state == "declared" {
						want = "404"
					}
					if got := strconv.Itoa(response.StatusCode); got != want {
						t.Errorf("the valid request to %s answered %s, want %s: %.120q", op.key(), got, want, raw)
					}
				}
				matrixRecord(findings, reasons, ops, v, response, raw)
				answers++
			}
		}
	}
	if strings.Contains(logs.String(), matrixLogLine) {
		findings["the server logged an answer it refused to send: "+matrixLogLine] = &matrixFinding{count: 1, example: "see the server's log"}
	}
	matrixReport(t, findings, answers)
}

// TestEveryAnswerOfPlaybackInfoWithNoWorkerIsDeclaredByTheContract asks the variants of PlaybackInfo for a film that needs a transcode
// in an env where no worker runs. Its answer is a 200 that carries the no-capacity reason beside Jellyfin's ErrorCode, and that
// reason must be declared for the 200 like any other.
func TestEveryAnswerOfPlaybackInfoWithNoWorkerIsDeclaredByTheContract(t *testing.T) {
	kit, source, _ := decisionEnv(t, refusalCase{}.film())
	mustExec(t, kit.pool, "TRUNCATE transcode_nodes")
	body := chrome(t)
	body["MediaSourceId"] = source
	noWorker, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	document := servedContract(t, kit)
	ops := matrixOps(t, document)
	reasons := reasonsOf(t, document)
	var logs matrixLogs
	previous := kit.apis[0].Log
	kit.apis[0].Log = slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { kit.apis[0].Log = previous })
	world := matrixWorld{base: kit.servers[0].URL, caller: db.CompactID(kit.user), callerToken: kit.token, other: db.CompactID(kit.other),
		item: db.CompactID(kit.item)}
	findings := map[string]*matrixFinding{}
	answers := 0
	for _, op := range ops {
		if op.method != http.MethodPost || !strings.Contains(op.path, "{itemId}") {
			continue
		}
		path := strings.ReplaceAll(op.path, "{itemId}", world.item)
		for _, v := range matrixVariants(world, op, "no worker", world.base, world.callerToken, path, noWorker) {
			response, raw := matrixSend(t, v)
			matrixRecord(findings, reasons, ops, v, response, raw)
			answers++
		}
	}
	if strings.Contains(logs.String(), matrixLogLine) {
		findings["the server logged an answer it refused to send: "+matrixLogLine] = &matrixFinding{count: 1, example: "see the server's log"}
	}
	matrixReport(t, findings, answers)
}
