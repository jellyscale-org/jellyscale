package playback

import "testing"

func tracks(spec ...string) []Stream {
	streams := []Stream{{Index: 0, Type: "Video", Codec: "h264"}}
	for i, tag := range spec {
		flagged := len(tag) > 0 && tag[len(tag)-1] == '*'
		streams = append(streams, Stream{Index: i + 1, Type: "Audio", Codec: "aac", Language: tag[:len(tag)-btoi(flagged)], IsDefault: flagged})
	}
	return streams
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestChooseAudio(t *testing.T) {
	two := Number(2)
	subtitle := Number(4)
	for _, test := range []struct {
		name   string
		film   []Stream
		saved  string
		play   bool
		named  *Number
		want   int
		wantOK bool
	}{
		{"no audio", tracks(), "deu", false, nil, 0, false},
		{"nothing saved", tracks("eng*", "deu"), "", false, nil, 1, true},
		{"a language the film has, play default off", tracks("eng*", "deu"), "deu", false, nil, 2, true},
		{"play default on keeps the flagged track", tracks("eng*", "deu"), "deu", true, nil, 1, true},
		{"play default on, the flagged track in the language", tracks("eng", "deu*", "deu*"), "ger", true, nil, 2, true},
		{"a spelling of the language", tracks("eng*", "de-DE"), "deu", false, nil, 2, true},
		{"a track with no language never matches", tracks("eng*", ""), "und", false, nil, 1, true},
		{"original language is the default track's", tracks("eng", "deu*", "deu"), "OriginalLanguage", false, nil, 2, true},
		{"unmatched language: the first track in the default's language", tracks("eng", "deu", "deu*"), "fra", false, nil, 2, true},
		{"a named audio track", tracks("eng*", "deu"), "eng", false, &two, 2, true},
		{"a named index that is no audio stream gives the file's default", append(tracks("eng", "deu*"), Stream{Index: 4, Type: "Subtitle"}), "eng", false, &subtitle, 2, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ChooseAudio(test.film, test.saved, test.play, test.named)
			if got != test.want || ok != test.wantOK {
				t.Errorf("got %d, %v; want %d, %v", got, ok, test.want, test.wantOK)
			}
		})
	}
}

// A track that is not the file's default is a transcode only for a profile whose IsSecondaryAudio condition applies to
// the track's codec: one that names other codecs, or has none, direct plays it.
func TestSecondaryAudioTranscodesOnlyWhereTheProfileSaysSo(t *testing.T) {
	source := copyable("mkv", "aac", "aac")
	condition := func(codec string) string {
		return `"CodecProfiles":[{"Type":"VideoAudio","Codec":"` + codec + `","Conditions":[{"Condition":"Equals","Property":"IsSecondaryAudio","Value":"false","IsRequired":false}]}]`
	}
	for name, test := range map[string]struct {
		profile string
		direct  bool
	}{
		"no condition":             {``, true},
		"a condition on any codec": {condition(""), false},
		"a condition on aac":       {condition("aac"), false},
		"a condition on ac3 only":  {condition("ac3"), true},
	} {
		body := `{"AudioStreamIndex": 2}`
		if test.profile != "" {
			body = `{"AudioStreamIndex": 2, "DeviceProfile": {"MaxStreamingBitrate": 120000000, "DirectPlayProfiles":[{"Container":"mkv","Type":"Video","VideoCodec":"h264","AudioCodec":"aac"}],
				"TranscodingProfiles":[{"Container":"mp4","Type":"Video","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls"}],` + test.profile + `}}`
		} else {
			body = `{"AudioStreamIndex": 2, "DeviceProfile": {"MaxStreamingBitrate": 120000000, "DirectPlayProfiles":[{"Container":"mkv","Type":"Video","VideoCodec":"h264","AudioCodec":"aac"}],
				"TranscodingProfiles":[{"Container":"mp4","Type":"Video","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls"}]}}`
		}
		parsed, err := ParseRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if d := Decide(parsed, source, true); d.DirectPlay != test.direct || d.AudioIndex != 2 {
			t.Errorf("%s: DirectPlay %v (reasons %v) with audio %d, want %v with audio 2", name, d.DirectPlay, d.Reasons, d.AudioIndex, test.direct)
		}
	}
}
