//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// JS-037: jellyscale is asked the requests of compat/captures/quick-connect-12.4.json (which
// test/differential/capture_quick_connect.py takes from a real Jellyfin 12.4), in its order, and each answer is held to
// Jellyfin's: the status; for a 200 the fields a client reads; and an error's body where it is a JSON string, which a
// client can read ("Quick connect is disabled"). Other error bodies are ASP.NET's own and are not compared. A
// difference must be listed in compat/differences.yaml under the request "quick connect: <step>", with the path
// "(status)" or "body.<field>"; an entry that no longer differs fails too.
//
// The capture turns Quick Connect off and on in the running server; jellyscale reads it from configuration, so the
// replay starts another replica, with the same database, where the capture changed the setting. The capture's disabled
// account is not replayed: jellyscale cannot disable an account before JS-033.

type qcStep struct {
	Section, Name, Action string
	Method, Path          string
	Headers               map[string]string
	Body, Saves, Pick     *string
	Status                int
	Response              any
	User, Device, Token   string
	Enabled, Disabled     bool
}

const qcReplayed = "quick connect: "

// Steps that ask what only a disabled account would show.
var qcNeedsDisabledAccounts = []string{"redeem after the approver is disabled", "the token redeemed after the approver was disabled"}

type qcReplay struct {
	*qcSite
	values   map[string]string // The capture's placeholder to jellyscale's value.
	accounts map[string]string // The capture's account name to the test's.
	at       int               // The replica asked.
}

var qcPlaceholder = regexp.MustCompile(`\{(code|secret|token|userid|userid-dashed):([\w-]+)\}`)

func (r *qcReplay) fill(text string) string {
	return qcPlaceholder.ReplaceAllStringFunc(text, func(placeholder string) string {
		parts := qcPlaceholder.FindStringSubmatch(placeholder)
		if parts[1] == "userid-dashed" {
			return strings.ToUpper(dashed(r.values["{userid:"+parts[2]+"}"]))
		}
		if parts[1] == "code" && parts[2] == "spaced-out" {
			code := r.values["{code:spaced}"]
			return code[:min(3, len(code))] + "%20" + code[min(3, len(code)):]
		}
		if value, found := r.values[placeholder]; found {
			return value
		}
		// jellyscale did not give what Jellyfin gave here (that difference is reported at that step).
		return "never-given-" + parts[2]
	})
}

// account is the test's account of the capture's name, and the capture's name of the test's.
func (r *qcReplay) captured(name string) string {
	for capture, ours := range r.accounts {
		if ours == name {
			return capture
		}
	}
	return name
}

func (r *qcReplay) addAccount(name string) {
	r.t.Helper()
	ours := "qc-" + name + "-" + uuid.NewString()[:8]
	id := uuid.NewString()
	hash, err := bcrypt.GenerateFromPassword([]byte(rightPassword), bcrypt.MinCost)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.pool.Exec(r.t.Context(), `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, $3, $4)`,
		id, ours, string(hash), name == "admin"); err != nil {
		r.t.Fatal(err)
	}
	r.accounts[name] = ours
	r.values["{userid:"+name+"}"] = strings.ReplaceAll(id, "-", "")
}

type qcDifference struct{ request, path string }

func (r *qcReplay) compare(step qcStep, got signInReply, note func(path, jellyfin, ours string)) {
	if got.status != step.Status {
		note("(status)", fmt.Sprint(step.Status), fmt.Sprintf("%d %.120q", got.status, got.body))
		return
	}
	if text, isText := step.Response.(string); step.Status != 200 {
		// A JSON string body, which the capture holds without its quotes and with a JSON content type.
		var ours string
		if isText && step.Response != "" && !strings.HasPrefix(text, "Error processing request") {
			if json.Unmarshal(got.body, &ours) != nil || ours != text {
				note("body", text, string(got.body))
			}
		}
		return
	}
	var ours any
	if err := json.Unmarshal(got.body, &ours); err != nil {
		note("body", fmt.Sprint(step.Response), string(got.body))
		return
	}
	switch theirs := step.Response.(type) {
	case map[string]any:
		mine, isObject := ours.(map[string]any)
		if !isObject {
			note("body", "an object", string(got.body))
			return
		}
		switch {
		case theirs["Secret"] != nil:
			r.compareFields(note, "body.", theirs, mine, []string{"Authenticated", "DeviceId", "DeviceName", "AppName", "AppVersion"}, []string{"Secret", "Code"})
			r.compareKeys(note, theirs, mine)
			if added, _ := mine["DateAdded"].(string); added == "" {
				note("body.DateAdded", "a time", fmt.Sprint(mine["DateAdded"]))
			} else if _, err := time.Parse(time.RFC3339Nano, added); err != nil {
				note("body.DateAdded", "a time", added)
			}
			if code, _ := mine["Code"].(string); !sixDigits.MatchString(code) {
				note("body.Code", "six digits", code)
			}
		case theirs["AccessToken"] != nil:
			r.compareKeys(note, theirs, mine)
			user, _ := mine["User"].(map[string]any)
			wantUser, _ := theirs["User"].(map[string]any)
			r.compareFields(note, "body.User.", wantUser, user, []string{"Name"}, []string{"Id"})
			info, _ := mine["SessionInfo"].(map[string]any)
			wantInfo, _ := theirs["SessionInfo"].(map[string]any)
			r.compareFields(note, "body.SessionInfo.", wantInfo, info, []string{"DeviceId", "DeviceName", "Client", "ApplicationVersion", "UserName"}, nil)
		default: // GET /Users/Me.
			r.compareFields(note, "body.", theirs, mine, []string{"Name"}, nil)
		}
	case []any: // The picked rows of /Sessions.
		mine, _ := ours.([]any)
		var picked []any
		for _, row := range mine {
			fields, _ := row.(map[string]any)
			if fields["DeviceId"] != *step.Pick {
				continue
			}
			kept := map[string]any{}
			for _, name := range []string{"DeviceId", "DeviceName", "Client", "ApplicationVersion", "UserName"} {
				kept[name] = fields[name]
			}
			kept["UserName"] = r.captured(fmt.Sprint(kept["UserName"]))
			picked = append(picked, kept)
		}
		if len(picked) != len(theirs) || len(picked) > 0 && !reflect.DeepEqual(picked, theirs) {
			note("body", fmt.Sprint(theirs), fmt.Sprint(picked))
		}
	default:
		if !reflect.DeepEqual(ours, theirs) {
			note("body", fmt.Sprint(theirs), fmt.Sprint(ours))
		}
	}
}

func (r *qcReplay) compareKeys(note func(path, jellyfin, ours string), theirs, mine map[string]any) {
	keys := func(object map[string]any) []string {
		var names []string
		for name := range object {
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	}
	if want, got := keys(theirs), keys(mine); !slices.Equal(want, got) {
		note("body.(fields)", strings.Join(want, " "), strings.Join(got, " "))
	}
}

// compareFields holds the named fields equal; placed are fields whose value the capture holds as a placeholder.
func (r *qcReplay) compareFields(note func(path, jellyfin, ours string), prefix string, theirs, mine map[string]any, same, placed []string) {
	for _, name := range same {
		got := mine[name]
		if name == "Name" || name == "UserName" {
			got = r.captured(fmt.Sprint(got))
		}
		if !reflect.DeepEqual(theirs[name], got) {
			note(prefix+name, fmt.Sprint(theirs[name]), fmt.Sprint(mine[name]))
		}
	}
	for _, name := range placed {
		want := r.fill(fmt.Sprint(theirs[name]))
		if fmt.Sprint(mine[name]) != want {
			note(prefix+name, fmt.Sprint(theirs[name])+" ("+want+")", fmt.Sprint(mine[name]))
		}
	}
}

// save keeps the secrets, codes and tokens jellyscale gave under the capture's names. A Connect or a sign-in is compared
// against them, so a new code is kept before the answer is compared.
func (r *qcReplay) save(step qcStep, got signInReply) {
	var reply struct{ Secret, Code, AccessToken string }
	if step.Saves == nil || got.status != 200 || json.Unmarshal(got.body, &reply) != nil {
		return
	}
	if reply.Secret != "" {
		r.values["{secret:"+*step.Saves+"}"], r.values["{code:"+*step.Saves+"}"] = reply.Secret, reply.Code
	}
	if reply.AccessToken != "" {
		r.values["{token:"+*step.Saves+"}"] = reply.AccessToken
	}
}

func TestJellyscaleAnswersTheCapturedQuickConnectRequests(t *testing.T) {
	raw, err := os.ReadFile("../../compat/captures/quick-connect-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct{ Steps []qcStep }
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	r := &qcReplay{qcSite: newQCSite(t, 2, nil), values: map[string]string{"{token:unknown}": "0123456789abcdef0123456789abcdef"},
		accounts: map[string]string{}}
	for _, name := range []string{"admin", "viewer", "other", "gone"} {
		r.addAccount(name)
	}
	found := map[qcDifference][]string{}
	replicas := []int{0, 1} // Those that have Quick Connect as the capture's server has it now.
	asked := 0
	for _, step := range capture.Steps {
		switch step.Action {
		case "set_enabled":
			extra := map[string]any{"quickConnect": step.Enabled}
			if step.Enabled {
				extra = nil
			}
			first := r.add(extra)
			replicas = []int{first, r.add(extra)}
			continue
		case "delete_user":
			if _, err := r.pool.Exec(t.Context(), "DELETE FROM users WHERE name = $1", r.accounts[step.User]); err != nil {
				t.Fatal(err)
			}
			continue
		case "set_disabled":
			continue
		case "password_signin":
			got := r.try(replicas[0], "dev-"+step.Device, r.accounts[step.User], rightPassword,
				"Authorization", qcDevice{id: "dev-" + step.Device, name: "TV-" + step.Device, app: "capture", version: "1.0"}.header(""))
			r.values["{token:"+step.Token+"}"] = signedIn(t, step.Name, got)
			continue
		case "request":
		default:
			t.Fatalf("the capture has the action %q, which the replay does not know", step.Action)
		}
		if slices.Contains(qcNeedsDisabledAccounts, step.Name) {
			continue
		}
		var headers []string
		for name, value := range step.Headers {
			if filled := r.fill(value); filled != "" {
				headers = append(headers, name, filled)
			}
		}
		body := ""
		if step.Body != nil {
			body = r.fill(*step.Body)
		}
		// Jellyfin lists a session from its sign-in; jellyscale once the device has reported, as for a password sign-in.
		if step.Pick != nil {
			r.call(replicas[0], "POST", "/Sessions/Playing/Ping", "", "X-Emby-Token", r.values["{token:asker}"])
		}
		asked++
		got := r.call(replicas[asked%len(replicas)], step.Method, r.fill(step.Path), body, headers...)
		r.save(step, got)
		request := qcReplayed + step.Name
		r.compare(step, got, func(path, jellyfin, ours string) {
			d := qcDifference{request, path}
			found[d] = append(found[d], fmt.Sprintf("Jellyfin %s, jellyscale %s", jellyfin, ours))
		})
	}
	if asked < 80 {
		t.Fatalf("the capture gave %d requests; it has over 80", asked)
	}

	listed, err := os.ReadFile("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Requests []string `yaml:"requests"`
		Path     string   `yaml:"path"`
		Paths    []string `yaml:"paths"`
	}
	if err := yaml.Unmarshal(listed, &entries); err != nil {
		t.Fatal(err)
	}
	allowed := map[qcDifference]bool{}
	for _, entry := range entries {
		for _, request := range entry.Requests {
			if !strings.HasPrefix(request, qcReplayed) {
				continue
			}
			for _, path := range append([]string{entry.Path}, entry.Paths...) {
				if path != "" {
					allowed[qcDifference{request, path}] = true
				}
			}
		}
	}
	for _, step := range capture.Steps {
		for d, what := range found {
			if d.request == qcReplayed+step.Name && !allowed[d] {
				t.Errorf("%s: %s differs from Jellyfin 12.4 and compat/differences.yaml does not list it: %s", d.request, d.path, what[0])
			}
		}
	}
	for d := range allowed {
		if _, differs := found[d]; !differs {
			t.Errorf("compat/differences.yaml lists %s of %q and jellyscale answers as Jellyfin does: remove the entry", d.path, d.request)
		}
	}
}
