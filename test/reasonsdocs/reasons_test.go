// Package reasonsdocs holds the one check of JS-153 that has to name a symbol the build adds, in a package of its own so
// that its not compiling today stops nothing else: the vocabulary of playback reasons the decision line, the counters and
// TranscodeReasons share (docs/running.md lists it; the code has it as playback.Reasons).
//
// It names playback.Reasons, a function that returns every name of the vocabulary, which the build must add. Today it does
// not compile, and that is its failure.
package reasonsdocs

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"jellyscale/internal/playback"
)

// Jellyfin's names are the TranscodeReason enum of compat/schema-12.4.json; jellyscale's own are not in it.
var jellyfin = strings.Fields(`ContainerNotSupported VideoCodecNotSupported AudioCodecNotSupported SubtitleCodecNotSupported
	SecondaryAudioNotSupported VideoProfileNotSupported VideoLevelNotSupported VideoResolutionNotSupported InterlacedVideoNotSupported
	AudioChannelsNotSupported ContainerBitrateExceedsLimit VideoRangeTypeNotSupported`)

var own = strings.Fields(`KeyframeIndexUnusable BitrateUnknownUnderCap VideoRangeNotCopyable VideoRangeUnknown
	NoWorkerRunning NoWorkerWithFreeCores TranscodeStoreMismatch RememberedFailure WorkersAtCopyBound NoWorkerOfThisOutputVersion
	NoTranscodeStore NoProbedDuration`)

func TestJellyfinsNamesAreInItsEnumAndJellyscalesOwnAreNot(t *testing.T) {
	raw, err := os.ReadFile("../../compat/schema-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Types map[string]struct{ Enum []string } `json:"types"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	enum := schema.Types["TranscodeReason"].Enum
	if len(enum) < 20 {
		t.Fatalf("no TranscodeReason enum in the schema: %v", enum)
	}
	for _, name := range jellyfin {
		if !slices.Contains(enum, name) {
			t.Errorf("%s is claimed as Jellyfin's and is not in its enum", name)
		}
	}
	for _, name := range own {
		if slices.Contains(enum, name) {
			t.Errorf("%s is claimed as jellyscale's own and Jellyfin has it: use Jellyfin's", name)
		}
	}
}

func TestTheCodesVocabularyIsExactlyTheOnesThatTheChecksPin(t *testing.T) {
	want := append(slices.Clone(jellyfin), own...)
	got := playback.Reasons()
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Errorf("playback.Reasons() is %v, the checks pin %v", got, want)
	}
}

// docs/running.md lists every reason of the vocabulary with what it means, and its list equals the code's: a row or a bullet
// that starts with the name in backticks, neither holding a name the other lacks.
var listed = regexp.MustCompile("(?m)^(?:\\| |- )`([A-Z][A-Za-z]+)`")

func TestDocsRunningListsTheCodesVocabularyAndNothingElse(t *testing.T) {
	raw, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	var docs []string
	for _, m := range listed.FindAllStringSubmatch(string(raw), -1) {
		docs = append(docs, m[1])
	}
	code := playback.Reasons()
	for _, name := range code {
		if !slices.Contains(docs, name) {
			t.Errorf("docs/running.md does not list the reason %s", name)
		}
	}
	for _, name := range docs {
		if !slices.Contains(code, name) {
			t.Errorf("docs/running.md lists %s, which the code does not have", name)
		}
	}
	// Each is explained: the row or bullet says something after the name.
	for _, name := range code {
		line := regexp.MustCompile("(?m)^(?:\\| |- )`" + name + "`(.*)$").FindStringSubmatch(string(raw))
		if line != nil && len(strings.Trim(line[1], " |-:")) < 20 {
			t.Errorf("docs/running.md says too little of %s: %q", name, line[1])
		}
	}
}
