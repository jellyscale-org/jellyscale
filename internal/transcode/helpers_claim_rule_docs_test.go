package transcode

import (
	"regexp"
	"strings"
	"testing"
)

// JS-168 regression (D-052): the docs say the claim rule. The behaviour is checked
// by helpers_claim_rule_integration_test.go; these read the words, in any wording,
// for the statements the criteria name.

func TestDomains58StatesTheClaimRuleAndCitesD052(t *testing.T) {
	text := flat(invariant(t, "58"))
	if !strings.Contains(text, "d-052") {
		t.Error("DOMAINS 58 does not cite D-052")
	}
	if strings.Contains(text, "after a second to any worker with the cores") {
		t.Error("DOMAINS 58 still says a helper goes 'after a second to any worker with the cores'")
	}
	if !regexp.MustCompile(`never (be )?claimed by a worker[^.]*not a helper`).MatchString(text) {
		t.Error("DOMAINS 58 does not say a helper is never claimed by a worker running a job of its spec that is not a helper")
	}
	if !regexp.MustCompile(`second[^.]*only helpers|only helpers[^.]*second`).MatchString(text) {
		t.Error("DOMAINS 58 does not say the one-second fallback is only for a worker running only helpers of the spec")
	}
	if !regexp.MustCompile(`(queued|queues)[^.]*only (where|when|if)[^.]*(may|can) claim|only (where|when|if)[^.]*(may|can) claim[^.]*(queued|queues)`).MatchString(text) {
		t.Error("DOMAINS 58 does not say a helper is queued only where a worker that may claim it has the cores")
	}
}

func TestTranscodingDocGivesTheLoneWorkerReasonAndTheExceptionAndCitesD052(t *testing.T) {
	var lone, record string
	for _, line := range strings.Split(doc(t, "docs/transcoding.md"), "\n") {
		if strings.Contains(line, "No helper can start with one worker") {
			lone = flat(line)
		}
		if strings.Contains(strings.ToLower(line), "js-168 (built)") {
			record = flat(line)
		}
	}
	if lone == "" {
		t.Fatal("docs/transcoding.md lost 'No helper can start with one worker'")
	}
	if strings.Contains(lone, "a lone worker holds no second job of the price") {
		t.Error("docs/transcoding.md still gives 'a lone worker holds no second job of the price' as the reason, which is false for a worker with cores to spare")
	}
	if !strings.Contains(lone, "d-052") {
		t.Error("the 'No helper can start with one worker' bullet does not cite D-052")
	}
	if !regexp.MustCompile(`never[^.]*(viewer|seek|failover)|not a helper`).MatchString(lone) {
		t.Error("the 'No helper can start with one worker' bullet does not give the rule as its reason (a helper is never claimed by a worker running a job of its spec that is not a helper)")
	}
	if record == "" {
		t.Fatal("docs/transcoding.md lost its record of JS-168 (built)")
	}
	if !strings.Contains(record, "d-052") {
		t.Error("the record of JS-168 does not cite D-052")
	}
	if !regexp.MustCompile(`never[^.]*(viewer's|seek's|failover's)|(viewer's|seek's|failover's)[^.]*never`).MatchString(record) {
		t.Error("the record of JS-168 says a helper is queued and claimed wherever a worker has its cost free without the exception (never on a worker running a viewer's, a seek's or a failover's job of its spec)")
	}
}
