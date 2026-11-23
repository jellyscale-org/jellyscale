package playback

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// JS-134: the fallback of an HDR copy (JS-088, JS-133) keeps the picture's range: 10 bits with the copy's tags and no tone
// mapping. These checks name nothing the build adds except where hdrFallback says so.

// hdrFallback is the spec of the encode that stands in for an HDR copy whose index was found wrong, spelled as
// internal/api's goOn spells it today: Recoded(codec, the source's tone map). If the build spells it another way
// (another argument, a field of the source), this is the one line of this file, and of its twin in internal/transcode, to
// follow it; nothing else here depends on the spelling.
func hdrFallback(copy Spec, codec, transfer string) Spec { return copy.Recoded(codec, transfer) }

// What the build chose is what the player was seen to take (compat/captures/hdr-fallback-12.4.md): a 10-bit fallback with
// the copy's tags, unless the player refused it (video.error, a dialog or no frames, in either direction), which goes to
// the owner and is not settled by the build sending another format. The record also names the commands that reproduce it
// and what ffprobe says of an HDR10 copy and of today's fallback of it.
func TestTheHDRFallbacksFormatFollowsTheRecordOfWhatJellyfinWebDid(t *testing.T) {
	raw, err := os.ReadFile("../../compat/captures/hdr-fallback-12.4.json")
	if err != nil {
		t.Fatalf("the observation is not recorded beside av1-fallback-12.4.md: %v", err)
	}
	type direction struct {
		Frames     int     `json:"frames"`
		VideoError *string `json:"videoError"`
		Dialog     bool    `json:"dialog"`
		Reached    float64 `json:"reached"`
	}
	type variant struct {
		Forward direction `json:"forward"`
		Back    direction `json:"back"`
	}
	var record struct {
		Scripts  []string `json:"scripts"`
		Choice   string   `json:"choice"`
		Observed map[string]struct {
			NotObserved string  `json:"notObserved"`
			Control     variant `json:"control"`
			Eight       variant `json:"eight"`
			Ten         variant `json:"ten"`
		} `json:"observed"`
		FFprobe map[string]map[string]struct {
			ToneMapped bool `json:"toneMapped"`
			Init       struct {
				PixFmt    string `json:"pixFmt"`
				BitDepth  int    `json:"bitDepth"`
				Primaries string `json:"primaries"`
				Transfer  string `json:"transfer"`
				Matrix    string `json:"matrix"`
				Range     string `json:"range"`
			} `json:"init"`
			Frames struct {
				PixFmt           string  `json:"pixFmt"`
				BitDepth         int     `json:"bitDepth"`
				Primaries        string  `json:"primaries"`
				Transfer         string  `json:"transfer"`
				Matrix           string  `json:"matrix"`
				Range            string  `json:"range"`
				MasteringDisplay *string `json:"masteringDisplay"`
				ContentLight     *string `json:"contentLightLevel"`
			} `json:"frames"`
		} `json:"ffprobe"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"hdr-fallback-spike.mjs", "hdr-fallback-streams.sh", "hdr-fallback-observe.sh"} {
		found := false
		for _, s := range record.Scripts {
			found = found || strings.HasSuffix(s, script)
		}
		if _, err := os.Stat("../../compat/captures/" + script); err != nil || !found {
			t.Errorf("the record does not name %s, or the script is not beside it: %v", script, err)
		}
	}
	// What ffprobe says: the copy of each codec is 10 bits PQ in BT.2020, today's fallback is said to be tone mapped and is
	// 8 bits BT.709, for the init segment and for the frames of each.
	for _, codec := range []string{"hevc", "av1"} {
		rows, ok := record.FFprobe[codec]
		if !ok || rows["copy"].Init.PixFmt == "" || rows["today"].Init.PixFmt == "" {
			t.Errorf("the record has no ffprobe of the %s copy and of today's fallback of it", codec)
			continue
		}
		if copy := rows["copy"]; copy.Init.PixFmt != "yuv420p10le" || copy.Init.BitDepth != 10 || copy.Init.Transfer != "smpte2084" || copy.Init.Primaries != "bt2020" || copy.Init.Matrix != "bt2020nc" || copy.Init.Range != "tv" ||
			copy.Frames.PixFmt != "yuv420p10le" || copy.Frames.Transfer != "smpte2084" || copy.Frames.MasteringDisplay == nil || copy.Frames.ContentLight == nil {
			t.Errorf("%s copy: the record's ffprobe is not that of an HDR10 copy with mastering display and content light level: %+v", codec, copy)
		}
		if today := rows["today"]; !today.ToneMapped || today.Init.PixFmt != "yuv420p" || today.Init.BitDepth != 8 || today.Init.Transfer != "bt709" || today.Frames.PixFmt != "yuv420p" || today.Frames.Transfer != "bt709" || today.Frames.Primaries == "" || today.Frames.Matrix == "" || today.Frames.Range == "" {
			t.Errorf("%s today's fallback: the record does not say it is tone mapped to 8 bits BT.709, in the init and the frames: %+v", codec, today)
		}
	}
	// The format is the one the rule gives: 10 bits with the copy's tags, unless the player refused it.
	refused := func(v variant) bool {
		for _, d := range []direction{v.Forward, v.Back} {
			if d.Frames == 0 || d.VideoError != nil || d.Dialog {
				return true
			}
		}
		return false
	}
	av1, ok := record.Observed["av1"]
	if !ok || av1.NotObserved != "" {
		t.Fatalf("an AV1 copy's fallback is always observed (the criterion), not %+v", record.Observed["av1"])
	}
	if refused(av1.Control) {
		t.Errorf("the control (the copy all through) was refused by the player: the observation proves nothing: %+v", av1.Control)
	}
	for codec, row := range record.Observed {
		if row.NotObserved != "" {
			if codec == "av1" || row.NotObserved == "" {
				t.Errorf("%s: not observed needs a reason", codec)
			}
			continue
		}
		if refused(row.Ten) {
			t.Errorf("the player refused a 10-bit %s fallback with the copy's tags (%+v): this goes to the owner, and the build does not send another format", codec, row.Ten)
		}
	}
	if record.Choice != "10bit-copy-tags" {
		t.Errorf("the record names %q, the rule gives \"10bit-copy-tags\" (10 bits with the copy's tags and no tone mapping)", record.Choice)
	}
	// Today's 8-bit fallback is recorded beside it, with what the player did, so a reader sees what was replaced.
	if av1.Eight.Forward.Reached == 0 && av1.Eight.Back.Reached == 0 {
		t.Errorf("the record has no row for today's 8-bit fallback under a 10-bit PQ copy")
	}
}

// The fallback of an HDR copy is an encode of its own: it never shares segments with the 8-bit fallbacks (SDR, or a tone
// mapped one), with the copy, with another transfer's, or with the other codecs'.
func TestAnHDRFallbackIsAnEncodeOfItsOwn(t *testing.T) {
	copied := Spec{Item: "a", Source: "b", ETag: "e", AudioIndex: 1, Width: 1920, Height: 1080, Copy: true}
	for _, codec := range []string{"hevc", "av1"} {
		pq, hlg, sdr := hdrFallback(copied, codec, PQ), hdrFallback(copied, codec, HLG), hdrFallback(copied, codec, "")
		for name, other := range map[string]Spec{"the copy": copied, "an SDR fallback": sdr, "an HLG fallback": hlg, "the other codec's": hdrFallback(copied, map[string]string{"hevc": "av1", "av1": "hevc"}[codec], PQ)} {
			if pq.Hash() == other.Hash() {
				t.Errorf("the %s HDR10 fallback shares a hash with %s", codec, name)
			}
		}
		if pq.Copy || pq.Recode != codec {
			t.Errorf("the %s fallback is Copy %v Recode %q", codec, pq.Copy, pq.Recode)
		}
	}
	// A 4K copy's fallback is 1920x1080 as before.
	for _, codec := range []string{"hevc", "av1"} {
		got := hdrFallback(Spec{Width: 3840, Height: 2160, Copy: true}, codec, PQ)
		if got.Width != 1920 || got.Height != 1080 || got.SourceHeight != 2160 {
			t.Errorf("a 4K %s HDR copy falls back to %dx%d from %d lines, want 1920x1080 from 2160", codec, got.Width, got.Height, got.SourceHeight)
		}
	}
}

// The SDR fallbacks keep the hash they had apart from the version: a spec that is no HDR copy's carries nothing new.
func TestAnSDRFallbacksSpecIsWhatItWas(t *testing.T) {
	copied := Spec{Item: "i", Source: "s", ETag: "e", AudioIndex: 1, Width: 1280, Height: 720, VideoBitrate: 1_000_000, Copy: true}
	for _, codec := range []string{"h264", "hevc", "av1"} {
		got := copied.Recoded(codec, "")
		if got.ToneMap != "" || got.Copy || got.Recode != codec {
			t.Errorf("the %s SDR fallback is %+v", codec, got)
		}
	}
}

// A worker of the release before (output version 6) never claims the fallback job of an HDR copy: a job records the
// OutputVersion of the replica that queued it and a worker claims only its own (invariant 59), so the version is raised.
func TestOutputVersionIsRaisedForTheHDRFallback(t *testing.T) {
	if OutputVersion <= 6 {
		t.Errorf("OutputVersion = %d, want more than 6: a worker that makes an 8-bit or tone mapped fallback of an HDR copy must not claim its job", OutputVersion)
	}
}

// A 4K HDR source is the dearer to fall back from: it is decoded at full size before it is scaled down.
func TestA4KHDRFallbackCostsNoLessThanA1080pOne(t *testing.T) {
	for _, codec := range []string{"hevc", "av1"} {
		a := hdrFallback(Spec{Width: 1920, Height: 1080, Copy: true}, codec, PQ).Cost()
		b := hdrFallback(Spec{Width: 3840, Height: 2160, Copy: true}, codec, PQ).Cost()
		if b < a {
			t.Errorf("a 4K %s HDR fallback costs %.2f, less than a 1080p one's %.2f", codec, b, a)
		}
	}
}
