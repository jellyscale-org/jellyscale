package api

import (
	"github.com/prometheus/client_golang/prometheus"

	"jellyscale/internal/signin"
)

// Not registered anywhere: Collectors hands them to whoever serves /metrics.
var (
	playbackDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jellyscale_playbackinfo_decisions_total",
		Help: "How PlaybackInfo said a source plays: direct_play, copy (the video as it is, the audio copied or re-encoded) or reencode. A source is only ever one of them; jellyscale has no separate direct stream."}, []string{"method"})
	playbackRefusals = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jellyscale_playbackinfo_refusals_total",
		Help: "Sources PlaybackInfo refused because the transcode they need cannot start now."})
	playbackFallbacks = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jellyscale_playback_copy_fallbacks_total",
		Help: "Streams of a copied video that went on as a re-encode because the file's keyframe index was found wrong, counted once for each stream."})
)

// The sign-in bounds (JS-145). No label names an account or an address: those would grow with what a guesser invents.
var (
	signinFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jellyscale_signin_failures_total",
		Help: "Sign-ins answered 401: a wrong password, or a name that is no account."})
	signinRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jellyscale_signin_refusals_total",
		Help: "Sign-ins refused with 403 because a bound was reached, by the bound: name, device or address."}, []string{"bound"})
	signinUnattributed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jellyscale_signin_unattributed_total",
		Help: "Sign-ins through a declared proxy with no usable forwarded address, to which the per-address bound was not applied."})
	signinAddressBound = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jellyscale_signin_address_bound",
		Help: "1 when the per-address sign-in bound is on (trustedProxies is set), 0 when it is off."})
)

func init() {
	for _, bound := range []string{signin.BoundName, signin.BoundDevice, signin.BoundAddress} {
		signinRefusals.WithLabelValues(bound)
	}
}

// Collectors are the metrics of this package.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{playbackDecisions, playbackRefusals, playbackFallbacks, signinFailures, signinRefusals, signinUnattributed, signinAddressBound}
}
