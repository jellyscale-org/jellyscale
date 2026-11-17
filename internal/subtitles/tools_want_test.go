package subtitles

// subtitleWant is what each run returned and logged, observed on the code
// before JS-112, for ffmpeg exiting 1 with a line on stderr, killed by a
// signal and cut off by its context.
var subtitleWant = map[string][3]string{
	"text": {
		"the file can't be read as subtitles: ffmpeg: exit status 1: boom: the file is not a film",
		"ffmpeg: signal: killed: ",
		"ffmpeg: signal: killed: ",
	},
	"sup": {
		"the file can't be read as subtitles: ffmpeg: exit status 1: boom: the file is not a film\n" +
			"level=INFO msg=\"sup extraction started\" item=item-1 source=in.mkv index=2 size=3\n" +
			"level=INFO msg=\"sup extraction ended\" item=item-1 source=in.mkv index=2 error=\"the file can't be read as subtitles: ffmpeg: exit status 1: boom: the file is not a film\"",
		"ffmpeg: signal: killed: \n" +
			"level=INFO msg=\"sup extraction started\" item=item-1 source=in.mkv index=2 size=3\n" +
			"level=INFO msg=\"sup extraction ended\" item=item-1 source=in.mkv index=2 error=\"ffmpeg: signal: killed: \"",
		"ffmpeg: signal: killed: \n" +
			"level=INFO msg=\"sup extraction started\" item=item-1 source=in.mkv index=2 size=3\n" +
			"level=INFO msg=\"sup extraction ended\" item=item-1 source=in.mkv index=2 error=\"ffmpeg: signal: killed: \"",
	},
	"fonts": {
		"<nil>\nlevel=ERROR msg=\"the film's fonts can't be taken out\" error=\"exit status 1\" output=\"boom: the file is not a film\"",
		"ffmpeg: signal: killed: \n",
		"ffmpeg: signal: killed: \n",
	},
}
