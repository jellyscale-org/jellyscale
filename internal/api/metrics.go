package api

import "github.com/prometheus/client_golang/prometheus"

// Not registered anywhere: Collectors hands them to whoever serves /metrics.
var (
	playbackDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jellyscale_playbackinfo_decisions_total",
		Help: "How PlaybackInfo said a source plays: direct_play, copy (the video as it is, the audio copied or re-encoded) or reencode. A source is only ever one of them; jellyscale has no separate direct stream."}, []string{"method"})
	playbackRefusals = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jellyscale_playbackinfo_refusals_total",
		Help: "Sources PlaybackInfo refused because the transcode they need cannot start now."})
)

// Collectors are the metrics of this package.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{playbackDecisions, playbackRefusals}
}
