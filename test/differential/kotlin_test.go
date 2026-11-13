package differential

// The requests whose answers the Kotlin SDK must decode (JS-070). Names the
// task implies:
//
//	Request{Kotlin string `yaml:"kotlin"`}
//	    the model of org.jellyfin.sdk.model.api that jellyscale's answer is decoded as by
//	    jellyfin-sdk-kotlin 1.9.4's own serializers ("BaseItemDto", "SessionInfoDto[]" for a list)

import (
	"strings"
	"testing"
)

func kotlinRequests(t *testing.T) map[string][]Request {
	t.Helper()
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string][]Request{}
	for _, r := range reqs {
		if r.Kotlin != "" {
			by[r.Kotlin] = append(by[r.Kotlin], r)
		}
	}
	return by
}

func TestTheRequestsCoverEveryAnswerTheKotlinSDKMustDecode(t *testing.T) {
	by := kotlinRequests(t)
	if n := len(by["SessionInfoDto[]"]); n < 2 {
		t.Errorf("%d requests decode GET /Sessions; want an idle session and one playing a transcode", n)
	}
	playing := false
	for _, r := range by["SessionInfoDto[]"] {
		playing = playing || (r.Play != "" && r.Route == "GET /Sessions")
	}
	if !playing {
		t.Error("no Sessions request decodes while a play is in progress (play:)")
	}
	var film, episode, series bool
	for _, r := range by["BaseItemDto"] {
		film = film || strings.Contains(r.Path, "{{Movie/")
		episode = episode || strings.Contains(r.Path, "{{Episode/")
		series = series || strings.Contains(r.Path, "{{Series/")
		if r.Route != "GET /Items/{itemId}" {
			t.Errorf("%s: a BaseItemDto request is GET /Items/{itemId}, not %s", r.Name, r.Route)
		}
	}
	if !film || !episode || !series {
		t.Errorf("BaseItemDto decoded for a film %v, an episode %v and a series %v; want all three", film, episode, series)
	}
	if len(by["BaseItemDtoQueryResult"]) == 0 {
		t.Error("no item listing is decoded as BaseItemDtoQueryResult")
	}
	info := by["PlaybackInfoResponse"]
	films := map[string]bool{}
	transcoding, direct := 0, 0
	for _, r := range info {
		if r.Route != "POST /Items/{itemId}/PlaybackInfo" {
			t.Errorf("%s: PlaybackInfoResponse is the answer of POST /Items/{itemId}/PlaybackInfo", r.Name)
		}
		films[r.Path[:strings.Index(r.Path, "/PlaybackInfo")]] = true
		if strings.Contains(r.Path, "MaxStreamingBitrate=1000000&") || strings.Contains(r.Path, "MaxStreamingBitrate=2000000&") {
			transcoding++
		} else {
			direct++
		}
		if !strings.Contains(r.Body, `"TranscodingProfiles":[{`) {
			t.Errorf("%s: sends no transcoding profile, so it cannot show a transcode answer", r.Name)
		}
	}
	// An H.264 and an HEVC film, each answered as direct play and as a transcode.
	if len(films) < 2 || transcoding < 2 || direct < 2 {
		t.Errorf("PlaybackInfo decoded for %d films, %d direct and %d transcoding requests; want 2 films, 2 and 2", len(films), direct, transcoding)
	}
	for _, r := range by["PlaybackInfoResponse"] {
		if r.Status != 0 && r.Status != 200 {
			t.Errorf("%s: decodes a status %d", r.Name, r.Status)
		}
	}
}

func TestEveryKotlinModelIsAClassOfTheSDK(t *testing.T) {
	known := map[string]bool{"BaseItemDto": true, "BaseItemDtoQueryResult": true, "PlaybackInfoResponse": true, "SessionInfoDto[]": true,
		"AuthenticationResult": true, "UserDto": true, "BaseItemDto[]": true, "PublicSystemInfo": true}
	for model, reqs := range kotlinRequests(t) {
		if !known[model] {
			t.Errorf("%s (%s): not a model this test knows; add it here if it is a class of jellyfin-model 1.9.4", model, reqs[0].Name)
		}
	}
}

// The #95 fields are lost only on the audio streams planPlayback adds from the full probe, which a film with one
// audio track has none of. Zeta Film has two (test/fixtures/posix.sh), so a transcode answer decoded for it shows a
// build that leaves the later streams bare; acceptance.sh puts that bug back and needs the tier to fail.
func TestSomeTranscodingPlaybackInfoDecodedIsForAFilmWithTwoAudioTracks(t *testing.T) {
	for _, r := range kotlinRequests(t)["PlaybackInfoResponse"] {
		if strings.Contains(r.Path, "{{Movie/Zeta Film (2023)}}") && (strings.Contains(r.Path, "MaxStreamingBitrate=1000000&") || strings.Contains(r.Path, "MaxStreamingBitrate=2000000&")) {
			return
		}
	}
	t.Error("no transcoding PlaybackInfo of Movie/Zeta Film (2023), the film with two audio tracks, is decoded")
}
