//go:build integration

package api

import "testing"

// With transcoding off or short of cores, the item names the default subtitle PlaybackInfo names with no track named: the
// mode chooses among every track and a choice nothing can deliver is no default on both, never the next track down the
// ranking. (A viewer's Play sends the item's track as a named one, which overrides the mode.)
func TestTheItemNamesWhatPlaybackInfoNamesWhereATrackCannotBeDelivered(t *testing.T) {
	for _, workers := range []string{"no store", "no worker", "no cores free"} {
		t.Run(workers, func(t *testing.T) {
			f := sdEnv(t, workers)
			f.cacheEveryOtherTextTrack(t)
			var wrong problems
			compared, listedOne := 0, false
			for _, mode := range []string{"Default", "Always", "OnlyForced", "Smart"} {
				for _, language := range []string{"", "eng", "deu"} {
					v := f.viewerWith(t, "v-"+mode+"-"+language, &settings{mode: mode, subtitle: language, playDefaultAudio: true})
					for name := range f.capture.Films {
						info := defaultSubtitle(f.ask(t, 0, v, f.film[name], true, nil))
						for _, alias := range []bool{false, true} {
							item := defaultSubtitle(sourceOf(f.item(t, 0, v, f.film[name], alias)))
							// The item may leave the field out where it names none.
							if item == -2 {
								item = -1
							}
							compared++
							listedOne = listedOne || info >= 0
							if item != info {
								wrong.addf("%s %s %q (alias %v): the item names %d, PlaybackInfo %d", name, mode, language, alias, item, info)
							}
						}
					}
				}
			}
			if !listedOne {
				t.Error("no cell named a track on PlaybackInfo: the check would pass with no default at all")
			}
			t.Logf("%d cells compared", compared)
			wrong.report(t)
		})
	}
}
