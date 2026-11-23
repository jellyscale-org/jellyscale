// An LD_PRELOAD interposer for the JS-159 checks. It holds ffmpeg at the rename that puts its playlist in place
// (a re-encode's vod playlist is written once, after the last segment's own rename), so that the real ffmpeg is
// alive with its last segment in place and its playlist not. When the playlist's rename comes it writes the names
// in the output directory to $JS159_HOLD_DIR/inplace, creates $JS159_HOLD_DIR/reached, and waits until
// $JS159_HOLD_DIR/release exists. Nothing else is changed.
#define _GNU_SOURCE
#include <dirent.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static int (*real_rename)(const char *, const char *);

int rename(const char *from, const char *to) {
	if (!real_rename)
		real_rename = dlsym(RTLD_NEXT, "rename");
	const char *hold = getenv("JS159_HOLD_DIR");
	const char *base = strrchr(to, '/');
	base = base ? base + 1 : to;
	static int held;
	if (hold && !held && strcmp(base, "index.m3u8") == 0) {
		held = 1;
		char path[4096], tmp[4096], out[4096];
		snprintf(out, sizeof out, "%s", to);
		char *slash = strrchr(out, '/');
		if (slash)
			*slash = 0;
		else
			strcpy(out, ".");
		snprintf(tmp, sizeof tmp, "%s/inplace.tmp", hold);
		snprintf(path, sizeof path, "%s/inplace", hold);
		FILE *f = fopen(tmp, "w");
		DIR *d = opendir(out);
		for (struct dirent *e; f && d && (e = readdir(d));)
			fprintf(f, "%s\n", e->d_name);
		if (d)
			closedir(d);
		if (f)
			fclose(f);
		real_rename(tmp, path);
		snprintf(path, sizeof path, "%s/reached", hold);
		f = fopen(path, "w");
		if (f)
			fclose(f);
		snprintf(path, sizeof path, "%s/release", hold);
		while (access(path, F_OK) != 0)
			usleep(20000);
	}
	return real_rename(from, to);
}
