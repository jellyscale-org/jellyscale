//go:build integration

package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// JS-145 (D-022): on the stack as shipped (this directory's compose.yaml, config.json and Caddyfile, the image built from
// this checkout), ten wrong passwords for ten invented names from the host, then the administrator's right password from a
// new device, signs in. Every client on the host reaches the ingress from the Docker bridge gateway's address, so a stack
// that counted per address would refuse the administrator here; with `trustedProxies` absent the per-address bound is off
// and the bounds per name and per known device (each name had one failure) refuse nothing.
//
// What differs from the stack as shipped, each because a test must not do otherwise: the ingress is published on a port
// Docker picks (the file binds 127.0.0.1:8096, which may be taken on the machine and must never be assumed free), /web is
// an empty directory (the web client's files are not needed to sign in, and a missing ../../web-dist would otherwise be
// created inside the checkout), and the project has a name of its own and is removed, images included, at the end. The
// network, its subnet, the ingress's address, the config and the Caddyfile are the shipped ones, so a subnet already in use
// on the machine fails the test, naming the network, rather than being changed.
func TestTheShippedStackLetsTheAdministratorInAfterTenWrongPasswordsFromTheHost(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Fatalf("this test needs docker with the compose plugin: docker compose version: %v", err)
	}
	failOnOverlappingNetwork(t)

	work := t.TempDir()
	media, web := filepath.Join(work, "media"), filepath.Join(work, "web")
	for _, dir := range []string{filepath.Join(media, "movies"), filepath.Join(media, "shows"), web} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	override := "services:\n  ingress:\n    ports: !override\n      - 127.0.0.1::8080\n"
	for _, service := range []string{"api", "api2", "stream", "stream2", "worker"} {
		override += fmt.Sprintf("  %s:\n    volumes:\n      - %s:/web:ro\n", service, web)
	}
	overridePath := filepath.Join(work, "override.yaml")
	if err := os.WriteFile(overridePath, []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	project := fmt.Sprintf("jsstack-%d-%d", os.Getpid(), rand.IntN(1<<30))
	password := fmt.Sprintf("admin-%d", rand.Int64())
	compose := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-p", project, "-f", filepath.Join(here, "compose.yaml"), "-f", overridePath}, args...)...)
		cmd.Env = append(os.Environ(), "MEDIA_DIR="+media, "JELLYSCALE_ADMIN_PASSWORD="+password, "JELLYSCALE_S3_ACCESS_KEY=", "JELLYSCALE_S3_SECRET_KEY=")
		return cmd.CombinedOutput()
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if out, err := compose(ctx, "down", "-v", "--rmi", "local", "--remove-orphans", "-t", "2"); err != nil {
			t.Errorf("docker compose down: %v\n%s", err, out)
		}
	})
	// Building the image and pulling Postgres and Caddy take minutes on a cold runner: this only bounds a hang.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if out, err := compose(ctx, "up", "-d", "--build"); err != nil {
		t.Fatalf("docker compose up: %v\n%s", err, out)
	}
	out, err := compose(ctx, "port", "ingress", "8080")
	if err != nil {
		t.Fatalf("docker compose port ingress 8080: %v\n%s", err, out)
	}
	base := "http://" + strings.TrimSpace(string(out))
	logs := func() string {
		out, _ := compose(context.Background(), "logs", "--no-color", "--tail", "60", "api", "api2")
		return string(out)
	}
	deadline := time.Now().Add(5 * time.Minute) // Bounds a hang; the stack is up within seconds once built.
	for {
		response, err := http.Get(base + "/System/Info/Public")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stack's ingress at %s did not answer /System/Info/Public with 200 (last: %v)\n%s", base, err, logs())
		}
		time.Sleep(500 * time.Millisecond)
	}

	var config struct {
		AdminUser string `json:"adminUser"`
	}
	raw, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &config); err != nil || config.AdminUser == "" {
		t.Fatalf("config.json names no adminUser: %v", err)
	}
	for i := 1; i <= 10; i++ {
		status, body := signIn(t, base, fmt.Sprintf("dev-%d-%d", i, rand.Int64()), fmt.Sprintf("nobody-%d-%d", i, rand.Int64()), "wrong")
		if status != 401 {
			t.Fatalf("wrong password %d of 10, for an invented name: %d %q, want 401\n%s", i, status, body, logs())
		}
	}
	status, body := signIn(t, base, fmt.Sprintf("dev-new-%d", rand.Int64()), config.AdminUser, password)
	var reply struct{ AccessToken string }
	if status != 200 || json.Unmarshal(body, &reply) != nil || reply.AccessToken == "" {
		t.Errorf("the administrator's right password from a new device after ten wrong ones from the host: %d %q, want a sign-in\n%s", status, body, logs())
	}

	// D-023: a refusal from the host, in the same stack, is logged, and the line says what its fields held. One invented
	// name failed from new devices until its bound refuses (a few attempts; 40 bounds a build that never refuses).
	refused := fmt.Sprintf("nobody-refused-%d", rand.Int64())
	for i := 1; ; i++ {
		status, body := signIn(t, base, fmt.Sprintf("dev-r-%d-%d", i, rand.Int64()), refused, "wrong")
		if status == 403 {
			break
		}
		if status != 401 || i == 40 {
			t.Fatalf("wrong password %d for %q: %d %q, want 401 until the bound per name refuses\n%s", i, refused, status, body, logs())
		}
	}
	all, _ := compose(ctx, "logs", "--no-color", "api", "api2")
	var line string
	for _, l := range strings.Split(string(all), "\n") {
		if strings.Contains(l, "sign-in refused") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("the stack logged no `sign-in refused` line for a refused sign-in\n%s", logs())
	}
	peer, forwarded := logField(line, "peer"), logField(line, "forwarded_unverified")
	t.Logf("the stack's refusal line: %s", line)
	t.Logf("in the shipped stack, from the host: peer=%q forwarded_unverified=%q", peer, forwarded)
	if peer == "" {
		t.Errorf("the line has no peer field: trustedProxies is absent in this stack, so no address was attributed: %s", line)
	}
	if logField(line, "address") != "" {
		t.Errorf("the line names address=%q in a stack that leaves trustedProxies out: %s", logField(line, "address"), line)
	}
	// Caddy writes the address it was connected from into X-Forwarded-For. From the host that is the Docker bridge
	// gateway, which is every client's, so it is the address of the network's gateway and no client's own. docs/running.md
	// says that (TestRunningDocSaysTheStacksLineIdentifiesNoClient).
	gateway, err := exec.Command("docker", "network", "inspect", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}", project+"_default").Output()
	if err != nil {
		t.Fatalf("docker network inspect: %v", err)
	}
	if forwarded == "" || !strings.Contains(string(gateway), forwarded) {
		t.Errorf("forwarded_unverified=%q, want the stack network's gateway (%s): the ingress sees every client at one address", forwarded, strings.TrimSpace(string(gateway)))
	}
}

// logField is the value of key in a text log line, unquoted, or "".
func logField(line, key string) string {
	m := regexp.MustCompile(`(?:^|[ |])` + key + `=("(?:[^"\\]|\\.)*"|\S*)`).FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	if v, err := strconv.Unquote(m[1]); err == nil {
		return v
	}
	return m[1]
}

func signIn(t *testing.T, base, device, user, password string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"Username": user, "Pw": password})
	request, _ := http.NewRequest("POST", base+"/Users/AuthenticateByName", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Client="c", Device="d", DeviceId="%s", Version="1"`, device))
	response, err := (&http.Client{Timeout: time.Minute}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, raw
}

// failOnOverlappingNetwork fails the test when a Docker network on the machine already holds a subnet that compose.yaml
// declares: compose would refuse to make the stack's network, and the test does not change the shipped subnet.
func failOnOverlappingNetwork(t *testing.T) {
	t.Helper()
	var stack struct {
		Networks map[string]struct {
			IPAM struct {
				Config []struct {
					Subnet string `yaml:"subnet"`
				} `yaml:"config"`
			} `yaml:"ipam"`
		} `yaml:"networks"`
	}
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &stack); err != nil {
		t.Fatal(err)
	}
	var declared []netip.Prefix
	for _, network := range stack.Networks {
		for _, c := range network.IPAM.Config {
			if prefix, err := netip.ParsePrefix(c.Subnet); err == nil {
				declared = append(declared, prefix)
			}
		}
	}
	if len(declared) == 0 {
		return
	}
	out, err := exec.Command("sh", "-c", `docker network ls -q | xargs -r docker network inspect --format '{{.Name}} {{range .IPAM.Config}}{{.Subnet}} {{end}}'`).Output()
	if err != nil {
		t.Fatalf("docker network inspect: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		for _, subnet := range fields[min(1, len(fields)):] {
			prefix, err := netip.ParsePrefix(subnet)
			if err != nil {
				continue
			}
			for _, d := range declared {
				if d.Overlaps(prefix) {
					t.Fatalf("the Docker network %s holds %s, which overlaps the subnet %s that deploy/compose/compose.yaml declares: the shipped stack cannot start here until it is removed", fields[0], prefix, d)
				}
			}
		}
	}
}
