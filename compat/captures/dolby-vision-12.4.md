# What Jellyfin 12.4 does with Dolby Vision and HDR10+ films

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (server 12.4.0, jellyfin-ffmpeg 8.1.3), one library of 15 synthetic films (640x360, 24 fps, 20 s, HEVC Main 10 or, for one, AV1, AAC, in MP4), data in `dolby-vision-12.4.json` (same directory). Made by `test/fixtures/e2e.sh` (`hdr_film`, `testenv.HDRFilm`): ffmpeg and x265 write an HDR10 (PQ), HLG or SDR picture, with the HDR10+ SEI where the name says Plus, and `test/fixtures/dovi` adds the Dolby Vision configuration box (`dvcC` up to profile 7, `dvvC` above) with the profile, the base layer compatibility id and the enhancement layer flag. There are no RPUs in the files: no tool here makes them, and Jellyfin labels a file from ffprobe's configuration record alone. The PlaybackInfo bodies hold the Chrome profile of `transcode-12.4.json` with HEVC (and AV1) added to the MP4 direct play and the fMP4 HLS profile, `VideoProfile main|main 10` and `VideoLevel 123` on HEVC as jellyfin-web writes them for a browser that plays HEVC Main 10, and the `VideoRangeType` list of the HEVC and AV1 CodecProfiles that jellyfin-web writes for five kinds of client (`deviceProfiles` in the JSON; the conditions are read from the bundle of the image, `main.jellyfin.bundle.js`).

## The labels (item DTO, and the same in PlaybackInfo's MediaStreams)

| File | `VideoRange` | `VideoRangeType` | `VideoDoViTitle` (and `DisplayTitle` after "360p HEVC ") |
|---|---|---|---|
| SDR | SDR | SDR | none; `SDR` |
| HDR10 (PQ) | HDR | HDR10 | none; `HDR` |
| HDR10+ | HDR | HDR10Plus | none; `HDR` |
| HLG | HDR | HLG | none; `HDR` |
| DV profile 5 (compat 0) | HDR | DOVI | `Dolby Vision Profile 5` |
| DV profile 7.6 (compat 6, enhancement layer) | HDR | DOVIWithEL | `Dolby Vision Profile 7.6 (HDR10)` |
| DV profile 7.6 with HDR10+ | HDR | DOVIWithELHDR10Plus | the same |
| DV profile 8.1 (compat 1) | HDR | DOVIWithHDR10 | `Dolby Vision Profile 8.1 (HDR10)` |
| DV profile 8.1 with HDR10+ | HDR | DOVIWithHDR10Plus | the same |
| DV profile 8.2 (compat 2, 8-bit BT.709 base) | **SDR** | DOVIWithSDR | `Dolby Vision Profile 8.2 (SDR)` |
| DV profile 8.4 (compat 4, HLG base) | HDR | DOVIWithHLG | `Dolby Vision Profile 8.4 (HLG)` |
| DV profile 8 with compat 0 | HDR | **DOVIInvalid** | `Dolby Vision Profile 8` |
| DV profile 8 with compat 3 | HDR | **HDR10** (the label falls back on the transfer function) | `Dolby Vision Profile 8.3` |
| DV profile 5 with HDR10+ | HDR | DOVI (HDR10+ is not added to profile 5) | `Dolby Vision Profile 5` |
| DV profile 10.1 (AV1, compat 1) | HDR | DOVIWithHDR10 | `Dolby Vision Profile 10.1 (HDR10)`, `360p AV1 ...` |

Where they differ from what the task assumed: profile 8 with compatibility id 0 is `DOVIInvalid`, not `DOVI`; id 3 is plain `HDR10`; profile 5 is `DOVI` whatever else the file has. The video stream also carries `DvVersionMajor`, `DvVersionMinor`, `DvProfile`, `DvLevel`, `RpuPresentFlag`, `ElPresentFlag`, `BlPresentFlag`, `DvBlSignalCompatibilityId` for a Dolby Vision file, and `Hdr10PlusPresentFlag: true` for one with HDR10+ metadata (none of them for other files). `DisplayTitle` is `360p HEVC SDR`, `360p HEVC HDR` or, for Dolby Vision, `360p HEVC` plus `VideoDoViTitle`. The HDR10+ metadata is in the frames' side data, not the stream's, so ffprobe has to read a frame to see it.

## What the decision does (`playbackInfo` in the JSON)

- A profile that lists a type plays a file of that type as it is. HDR10+ is HDR10 with more: a list with `HDR10` and no `HDR10Plus` takes an HDR10+ file.
- A Dolby Vision file whose type the profile does not list, but whose base layer the profile plays, is **not** direct played or direct streamed. The reply is a transcode with `TranscodeReasons=VideoRangeTypeNotSupported`, and the master playlist has two variants: first the file's own video (`-codec:v:0 copy -tag:v:0 hvc1 -bsf:v hevc_mp4toannexb`, the Dolby Vision box dropped), then an H.264 tone-mapped variant (`VIDEO-RANGE=SDR`, `avc1.640033`). So an HDR10 client of a profile 8.1 film gets the HDR10 picture, and one that wants SDR still can pick the second.
- That holds for profile 8.1 and 8.1 with HDR10+ (`DOVIWithHDR10`, `DOVIWithHDR10Plus`), 8.4 (`DOVIWithHLG`), 8.2 (`DOVIWithSDR`, whose copy is labelled SDR), profile 7 (`DOVIWithEL` and `DOVIWithELHDR10Plus`: the enhancement layer is dropped), `DOVIInvalid`, and profile 10.1 in AV1.
- **Profile 5 is never copied or played by a profile without `DOVI`**: the master playlist has the H.264 variant only, for every profile but the two that list `DOVI` (those direct play it).
- The master playlist's first variant names the copy:

| File | `VIDEO-RANGE` | `CODECS` | `SUPPLEMENTAL-CODECS` |
|---|---|---|---|
| 8.1 | PQ | `hvc1.2.4.L63.B0,mp4a.40.2` | `dvh1.08.10/db1p` |
| 8.1 with HDR10+ | PQ | `hvc1.2.4.L63.B0,mp4a.40.2` | `dvh1.08.10/db1p` |
| 8.4 | HLG | `hvc1.2.4.L63.B0,mp4a.40.2` | `dvh1.08.10/db4h` |
| 8.2 | SDR | `hvc1.1.4.L63.B0,mp4a.40.2` | none |
| 7.6 | PQ | `hvc1.2.4.L63.B0,mp4a.40.2` | none |
| 7.6 with HDR10+ | PQ | `hvc1.2.4.L63.B0,mp4a.40.2` | `hvc1.2.4.L63.B0/cdm4` |
| 8 with compat 0 (`DOVIInvalid`) | PQ | `hvc1.2.4.L63.B0,mp4a.40.2` | none |
| 10.1 (AV1) | PQ | `av01.0.01M.10,mp4a.40.2` | `dav1.10.10/db1p` |

The level in the DV brand (`dvh1.08.10`) is the file's `DvLevel`, and `L63` is ffprobe's level. The brand after the slash is `db1p` for an HDR10 base, `db4h` for HLG; a profile 7 file's enhancement layer is not named, and with HDR10+ metadata the supplemental codec is the base's own codec with `cdm4`.

## What jellyscale does, and where it differs

Today every Dolby Vision file is labelled `DOVI` (the item has no range labels at all), HDR10+ is HDR10, and an HDR film is never copied (`hdr-tonemap-12.4.md`, `hevc-av1-12.4.md`), so a profile 8.1 film is re-encoded in software for a client that would play its HDR10 base layer. The checks in `internal/playback/dolbyvision_test.go`, `internal/library`, `internal/api/dolbyvision_integration_test.go` and the differential tier pin the behaviour above, with two limits that are deliberate and unchanged: an HDR10 or HLG film with no Dolby Vision is not copied (`hevc-av1-12.4.md`: that is the HDR copy task, and `TestHDRH264IsNotCopied` and the like still hold), and Main is written `hvc1.1.6` where Jellyfin writes `hvc1.1.4`. The second, tone-mapped variant is not required of the copy's master playlist.
