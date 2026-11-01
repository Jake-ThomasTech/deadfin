// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"testing"
)

// A restarted keyframe-cut job seeks inside the start GOP. Accurate seek
// would drop the decoded audio before the seek point while the copied video
// starts at the keyframe, so each restart would open with an audio gap of up
// to 0.5 s. The job seeks without accurate seek instead (deadfin#799).
func TestBuildHLSArgsKeyframeCutRestartSeeksWithoutAccurateSeek(t *testing.T) {
	job := lateAudioHLSJob("aac")
	job.PadAudioStart = false
	job.KeyframeCuts = true
	job.StartNumber = 23
	job.KeyframeStartTicks = 1_288_000_000
	job.StartOffsetTicks = 1_293_000_000

	args := mustBuildHLSArgs(t, job)
	noAccurate, seek, input := argumentIndex(t, args, "-noaccurate_seek"), argumentIndex(t, args, "-ss"), argumentIndex(t, args, "-i")
	if noAccurate >= input || seek >= input {
		t.Fatalf("argv = %q, want -noaccurate_seek and -ss as input options", args)
	}
	if got := args[seek+1]; got != "00:02:09.300" {
		t.Errorf("-ss = %q, want the seek inside the GOP", got)
	}
	if countArgument(args, "-noaccurate_seek") != 1 {
		t.Errorf("argv = %q, want one -noaccurate_seek", args)
	}
}

func TestBuildHLSArgsKeepsAccurateSeekOutsideKeyframeCutRestarts(t *testing.T) {
	first := lateAudioHLSJob("aac")
	first.PadAudioStart = false
	first.KeyframeCuts = true
	remux := lateAudioHLSJob("aac")
	remux.PadAudioStart = false
	remux.StartOffsetTicks = 1_290_000_000
	transcode := hlsUnitJob()
	transcode.StartOffsetTicks = 1_290_000_000
	for name, job := range map[string]HLSJob{"keyframe-cut first GOP": first, "segment-length remux": remux, "transcode": transcode} {
		if args := mustBuildHLSArgs(t, job); countArgument(args, "-noaccurate_seek") != 0 {
			t.Errorf("%s argv = %q, want accurate seek", name, args)
		}
	}
}

// The late-audio pad of a restarted keyframe-cut job starts at the GOP start,
// where the audio now starts. A first_pts at the seek point would trim the
// audio before it again.
func TestBuildHLSArgsPadsKeyframeCutRestartFromTheGOPStart(t *testing.T) {
	job := lateAudioHLSJob("aac")
	job.KeyframeCuts = true
	job.StartNumber = 23
	job.KeyframeStartTicks = 1_288_000_000
	job.StartOffsetTicks = 1_293_000_000

	args := mustBuildHLSArgs(t, job)
	if got, want := args[argumentIndex(t, args, "-af")+1], "aresample=osr=48000:async=1:first_pts=6182400"; got != want {
		t.Errorf("-af = %q, want %q", got, want)
	}
}
