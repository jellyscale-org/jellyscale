package compose

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// D-022 (amending D-020): the shipped stack leaves `trustedProxies` absent, so docs/running.md says the per-address bound is
// off in that stack and why (every client reaches its ingress from the Docker bridge gateway's address), and that the bounds
// per name and per known device hold there. This replaces TestRunningDocSaysTheShippedStackSharesOneClientAddress, which
// held the shared-address lockout as what the shipped stack did.
func TestRunningDocSaysThePerAddressBoundIsOffInTheShippedStackAndWhy(t *testing.T) {
	raw, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	// The three things are said in one paragraph: the bound is off in the stack, why (the gateway), and that the bounds per
	// name and per known device hold. The criterion's hyphenated spellings count too.
	perAddress := regexp.MustCompile(`per[ -]address|address bound`)
	perName := regexp.MustCompile(`per[ -]name`)
	knownDevice := regexp.MustCompile(`known[ -]device`)
	found := false
	for _, paragraph := range strings.Split(strings.ToLower(string(raw)), "\n\n") {
		var off, why, hold bool
		for _, s := range regexp.MustCompile(`[.;]\s+`).Split(paragraph, -1) {
			stack := strings.Contains(s, "stack") || strings.Contains(s, "compose")
			off = off || stack && perAddress.MatchString(s) && strings.Contains(s, "off")
			why = why || strings.Contains(s, "gateway")
			hold = hold || perName.MatchString(s) && knownDevice.MatchString(s) && strings.Contains(s, "hold")
		}
		if off && why && hold {
			found = true
		}
	}
	if !found {
		t.Error("docs/running.md has no paragraph that says the per-address bound is off in the shipped stack, why (every client reaches its ingress from the Docker gateway's address) and that the bounds per name and per known device hold")
	}
	if strings.Contains(string(raw), "bound=address address=172.28.0.1") {
		t.Error("docs/running.md still shows the shipped stack refusing sign-ins by its gateway's address, which D-022 turns off")
	}
}

// runningDoc is docs/running.md lowercased, with its paragraphs and the sentences of each.
func runningDoc(t *testing.T) (raw string, paragraphs [][]string) {
	t.Helper()
	b, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	raw = strings.ToLower(string(b))
	for _, p := range strings.Split(raw, "\n\n") {
		paragraphs = append(paragraphs, regexp.MustCompile(`[.;]\s+|\n`).Split(p, -1))
	}
	return raw, paragraphs
}

// D-023: in the shipped stack the `sign-in refused` line carries the ingress's address as the peer and the Docker gateway
// in the forwarded field (TestTheShippedStackLetsTheAdministratorInAfterTenWrongPasswordsFromTheHost records both), neither
// a client's own. docs/running.md says so in one paragraph about that stack: the line identifies the account and the bound
// but not the client, the peer's address is every viewer's and must never be blocked, and the client's address is known
// only to whatever stands in front of the stack.
func TestRunningDocSaysTheShippedStacksRefusalLineIdentifiesNoClient(t *testing.T) {
	_, paragraphs := runningDoc(t)
	notClient := regexp.MustCompile(`not the client|no client|neither .*client|nor the client|not which client|not the viewer`)
	neverBlock := regexp.MustCompile(`never (be )?(block|ban|refus|firewall)`)
	inFront := regexp.MustCompile(`in front of|upstream of|before the stack|outside the stack`)
	for _, p := range paragraphs {
		var ident, block, front bool
		stack := false
		for _, s := range p {
			stack = stack || strings.Contains(s, "stack") || strings.Contains(s, "compose")
			ident = ident || strings.Contains(s, "account") && strings.Contains(s, "bound") && notClient.MatchString(s)
			block = block || neverBlock.MatchString(s) && (strings.Contains(s, "peer") || strings.Contains(s, "address"))
			front = front || inFront.MatchString(s) && strings.Contains(s, "client") && strings.Contains(s, "only")
		}
		if stack && ident && block && front {
			return
		}
	}
	t.Error("docs/running.md has no paragraph about the shipped stack that says its `sign-in refused` line identifies the account and the bound but not the client, that the peer's address is every viewer's and must never be blocked, and that the client's address is known only to whatever stands in front of the stack")
}

// D-023: a declared ingress needs an address that survives its container being recreated (or its network's prefix
// declared), and compose.yaml's comment on trustedProxies points to that paragraph.
func TestRunningDocSaysHowADeclaredIngressKeepsItsAddressAndComposePointsToIt(t *testing.T) {
	_, paragraphs := runningDoc(t)
	found := false
	for _, p := range paragraphs {
		for _, s := range p {
			if strings.Contains(s, "recreat") && regexp.MustCompile(`fixed address|static address|ipv4_address|prefix|subnet|stable address`).MatchString(s) {
				found = true
			}
		}
	}
	if !found {
		t.Error("docs/running.md does not say, in one sentence, how a declared ingress gets an address that survives its container being recreated (a fixed address) or that its network's prefix can be declared instead")
	}
	b, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var comment []string
	on := false
	for _, l := range strings.Split(string(b), "\n") {
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "#") && strings.Contains(trim, "trustedProxies") {
			on = true
		}
		if on && !strings.HasPrefix(trim, "#") {
			break
		}
		if on {
			comment = append(comment, strings.ToLower(trim))
		}
	}
	text := strings.Join(comment, " ")
	if !strings.Contains(text, "docs/running.md") || !regexp.MustCompile(`recreat|fixed address|prefix`).MatchString(text) {
		t.Errorf("compose.yaml's comment on trustedProxies must point to docs/running.md's paragraph on an ingress address that survives recreation (naming recreation, a fixed address or the prefix); it says: %q", text)
	}
}

// D-023: a server with no ingress declares `trustedProxies: []`, which makes the peer count and stops the start-up warning.
func TestRunningDocSaysAServerWithNoIngressDeclaresAnEmptyTrustedProxies(t *testing.T) {
	_, paragraphs := runningDoc(t)
	for _, p := range paragraphs {
		for _, s := range p {
			if strings.Contains(s, "[]") && strings.Contains(s, "warning") && strings.Contains(s, "peer") && regexp.MustCompile(`no ingress|without an ingress|not behind|nothing in front|no proxy in front`).MatchString(s) {
				return
			}
		}
	}
	t.Error("docs/running.md has no sentence that says a server with no ingress declares `[]` as trustedProxies so that the peer counts and the start-up warning stops")
}

// D-023: viewers behind one household NAT share one counted address.
func TestRunningDocSaysViewersBehindOneHouseholdNATShareACountedAddress(t *testing.T) {
	_, paragraphs := runningDoc(t)
	for _, p := range paragraphs {
		for _, s := range p {
			if regexp.MustCompile(`\bnat\b`).MatchString(s) && regexp.MustCompile(`share|same|one address`).MatchString(s) && strings.Contains(s, "address") {
				return
			}
		}
	}
	t.Error("docs/running.md has no sentence that says viewers behind one household NAT share one counted address")
}

// D-023: a forwarded entry must be a bare address; one with a port or brackets is unusable and the per-address bound is
// not applied to that request.
func TestRunningDocSaysAForwardedEntryMustBeABareAddress(t *testing.T) {
	_, paragraphs := runningDoc(t)
	for _, p := range paragraphs {
		var bare, unusable bool
		for _, s := range p {
			bare = bare || strings.Contains(s, "port") && strings.Contains(s, "bracket") && regexp.MustCompile(`bare|plain|only an address`).MatchString(s)
			unusable = unusable || strings.Contains(s, "unusable") && strings.Contains(s, "per-address") || strings.Contains(s, "unusable") && strings.Contains(s, "per address")
		}
		if bare && unusable {
			return
		}
	}
	t.Error("docs/running.md has no paragraph that says a forwarded entry must be a bare address, that one written with a port or brackets is unusable, and that the per-address bound is not applied to that request")
}

// D-023: docs/running.md nowhere says the `sign-in refused` line names the client's address without saying when: a sentence
// about the line and an address says which address (the one counted, the peer) or under what condition.
func TestRunningDocNeverSaysTheRefusalLineNamesAnAddressWithoutSayingWhen(t *testing.T) {
	_, paragraphs := runningDoc(t)
	qualifier := regexp.MustCompile(`\bwhen\b|\bonly\b|\bpeer\b|\bunless\b|\bif\b|counts|counted|attributed|\bno address|\bno usable`)
	for _, p := range paragraphs {
		for _, s := range p {
			if strings.Contains(s, "sign-in refused") && strings.Contains(s, "address") && !qualifier.MatchString(s) {
				t.Errorf("docs/running.md says the `sign-in refused` line names an address without saying which or when: %q", strings.TrimSpace(s))
			}
		}
	}
}
