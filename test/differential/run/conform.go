package main

// The schema checks of the tier (JS-070): every JSON answer of jellyscale (and of Jellyfin, to
// show the rules are none Jellyfin's own answers break) is held to Jellyfin 12.4's schema as it
// arrives, and the answers jellyfin-sdk-kotlin must decode and the server discovery of
// @jellyfin/sdk are run on the finished stack. compat/differences.yaml never excuses a violation.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"jellyscale/compat/conform"

	"gopkg.in/yaml.v3"
)

func loadConformance(schemaFile string) (*conform.Schema, conform.Rules, conform.Clients) {
	file, err := os.ReadFile(schemaFile)
	check(err)
	schema, err := conform.ParseSchema(file)
	check(err)
	rules, err := conform.LoadRules("compat/conform.yaml")
	check(err)
	clients, err := conform.LoadClients("compat/clients.yaml")
	check(err)
	return schema, rules, clients
}

// record holds an answer to the schema, if the server has a checker.
func (s *server) record(method, path string, status int, contentType string, data []byte) {
	if s.conform == nil {
		return
	}
	label := s.label
	if label == "" {
		label = method + " " + path
	}
	s.found = append(s.found, s.conform.Check(label, method, path, status, contentType, data)...)
}

// violations are what the checker found in the server's answers so far.
func (s *server) violations() []conform.Violation { return s.found }

// checkSchemaDrift fails when the checked-in schema is not what the running Jellyfin's OpenAPI
// document gives: a hand edit, or a Jellyfin that has changed.
func checkSchemaDrift(jf *server, schemaFile, image string) error {
	status, _, data, err := jf.send("GET", "/api-docs/openapi.json", "")
	if err != nil || status != 200 {
		return fmt.Errorf("jellyfin gave no OpenAPI document (status %d): %v", status, err)
	}
	generated, err := conform.FromOpenAPI(data, image)
	if err != nil {
		return err
	}
	checkedIn, err := os.ReadFile(schemaFile)
	if err != nil {
		return err
	}
	if err := conform.Drift(checkedIn, generated); err != nil {
		return fmt.Errorf("%s: %w", schemaFile, err)
	}
	fmt.Printf("schema: %s is the OpenAPI document of %s\n", schemaFile, image)
	return nil
}

// reportConformance runs what is left of JS-070 after the comparison, prints it, and returns how many failures it found.
func reportConformance(jf, js *server, clients conform.Clients) int {
	failed := 0
	fail := func(format string, args ...any) {
		failed++
		fmt.Printf("FAIL  "+format+"\n", args...)
	}
	served, err := servedRoutes("compat/endpoints.yaml")
	check(err)
	sent, failures := js.sweep(clients, served, jf)
	fmt.Printf("clients: sent %d requests\n", sent)
	for _, f := range failures {
		fail("clients: %s", f)
	}
	for _, f := range js.kotlin.decode() {
		fail("kotlin: %s", f)
	}
	js.kotlin.remove()
	if out, err := exec.Command("node", "test/sdknode/discover.mjs", js.base).CombinedOutput(); err != nil {
		fail("sdk: %s%v", out, err)
	} else {
		fmt.Printf("sdk: %s", out)
	}
	for _, s := range []*server{js, jf} {
		for _, line := range distinct(s.violations()) {
			if s == jf {
				fail("the schema check breaks on Jellyfin's own answer, so the rule is wrong: %s", line)
			} else {
				fail("schema: %s", line)
			}
		}
	}
	for _, s := range []struct {
		label  string
		server *server
	}{{"differential", js}, {"jellyfin", jf}} {
		summary, err := s.server.conform.Summary(s.label)
		if err != nil {
			fail("%v", err)
			continue
		}
		fmt.Println(summary)
	}
	return failed
}

// distinct lists each violation once, the first sixty; an array of a hundred films breaks one way a hundred times.
func distinct(vs []conform.Violation) []string {
	var lines []string
	seen := map[string]bool{}
	for _, v := range vs {
		key := v.Route + "|" + stripIndexes(v.JSONPath) + "|" + v.Field + "|" + v.Message
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(lines) == 60 {
			lines = append(lines, "... more violations, after the first 60 distinct ones")
			break
		}
		lines = append(lines, v.String())
	}
	return lines
}

func stripIndexes(path string) string {
	var b strings.Builder
	skip := false
	for _, r := range path {
		switch {
		case r == '[':
			skip = true
		case r == ']':
			skip = false
		case !skip:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// servedRoutes reads compat/endpoints.yaml: which "METHOD route" jellyscale serves, whatever the case and parameter names.
func servedRoutes(path string) (func(method, route string) bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var endpoints struct {
		Routes []struct{ Method, Path string }
	}
	if err := yaml.Unmarshal(raw, &endpoints); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range endpoints.Routes {
		have[routeKey(r.Method, r.Path)] = true
	}
	return func(method, route string) bool { return have[routeKey(method, route)] }, nil
}

func routeKey(method, route string) string {
	var b strings.Builder
	for _, seg := range strings.Split(strings.ToLower(route), "/") {
		if strings.HasPrefix(seg, "{") {
			seg = "{}"
		}
		b.WriteString("/" + seg)
	}
	return method + b.String()
}

// kotlinCases are the answers of jellyscale that jellyfin-sdk-kotlin must decode, kept in files for test/sdkjvm/decode.sh.
type kotlinCases struct {
	dir   string
	cases []kotlinCase
}

type kotlinCase struct{ model, name, file string }

func newKotlinCases() *kotlinCases {
	dir, err := os.MkdirTemp("", "differential-kotlin")
	check(err)
	return &kotlinCases{dir: dir}
}

func (k *kotlinCases) remove() { os.RemoveAll(k.dir) }

func (k *kotlinCases) add(model, name string, data []byte) error {
	file := filepath.Join(k.dir, fmt.Sprintf("%03d.json", len(k.cases)))
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return err
	}
	k.cases = append(k.cases, kotlinCase{model, name, file})
	return nil
}

// decode runs jellyfin-sdk-kotlin's serializers over every case at once and returns what failed,
// each as the model, the request and the serializer's message, which names the JSON path and the field.
func (k *kotlinCases) decode() []string {
	if len(k.cases) == 0 {
		return []string{"no answer was kept for the Kotlin SDK to decode"}
	}
	args := []string{"test/sdkjvm/decode.sh"}
	for _, c := range k.cases {
		args = append(args, c.model, c.file)
	}
	out, err := exec.Command("bash", args...).CombinedOutput()
	var failures []string
	decoded := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		switch {
		case strings.HasPrefix(line, "decoded "):
			decoded++
		case strings.HasPrefix(line, "FAILED "):
			for _, c := range k.cases {
				line = strings.Replace(line, " "+c.file+":", fmt.Sprintf(" for the answer to %q:", c.name), 1)
			}
			failures = append(failures, line)
		default:
			if line != "" {
				failures = append(failures, line)
			}
		}
	}
	if err != nil && len(failures) == 0 {
		failures = append(failures, err.Error())
	}
	if len(failures) == 0 && decoded != len(k.cases) {
		failures = append(failures, fmt.Sprintf("decoded %d of %d answers", decoded, len(k.cases)))
	}
	if len(failures) == 0 {
		fmt.Printf("kotlin: decoded %d answers\n", decoded)
	}
	return failures
}
