// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"strings"
	"testing"
)

// The hardware goldens are the argv that the pinned
// MediaEncoder.ExtractVideoImagesOnIntervalAccelerated builds with
// EncodingHelper.GetMjpegEncoder and the MJPEG branches of the hardware
// filter chains, for a 1080p H.264 SDR source, a 320 wide rendition, a 10 s
// interval, ProcessThreads 1, and Qscale 4.

const (
	trickplayTiming   = "setpts=N/30.000/TB,fps=1000/10000:round=up"
	trickplayBT709    = "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709"
	trickplayImageOut = "-fps_mode passthrough -f image2 /cache/trickplay/%08d.jpg"
)

func trickplayJob(hardware HardwareOptions) TrickplayExtraction {
	return TrickplayExtraction{
		InputPath:                 "/media/movies/a.mkv",
		OutputPattern:             "/cache/trickplay/%08d.jpg",
		MaxWidth:                  320,
		IntervalMilliseconds:      10000,
		Threads:                   1,
		QualityScale:              4,
		AllowHardwareAcceleration: true,
		EnableHardwareEncoding:    true,
		Hardware:                  hardware,
		VideoStream:               VideoStream{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, PixelFormat: "yuv420p", Profile: "High"},
		AspectRatio:               "16:9",
	}
}

func intelMjpegCapabilities() Capabilities {
	caps := jellyfinFFmpegCapabilities()
	caps.Encoders = append(caps.Encoders, "mjpeg_qsv", "mjpeg_vaapi")
	caps.VaapiDriver = "iHD"
	return caps
}

func softwareTrickplayArgs() string {
	return "-hide_banner -loglevel error -nostdin -hwaccel none -threads 1 -i /media/movies/a.mkv -an -sn -vf " +
		trickplayTiming + "," + TrickplayScaleFilter(320) +
		" -threads 1 -strict unofficial -c:v mjpeg -q:v 4 " + trickplayImageOut
}

func TestBuildTrickplayArgs(t *testing.T) {
	qsvNoEncoding := qsvOptions()
	qsvNoEncoding.EnableHardwareEncoding = false
	vaapi := qsvOptions()
	vaapi.Type = HardwareAccelerationVAAPI
	tests := []struct {
		name string
		job  func() TrickplayExtraction
		caps Capabilities
		want string
	}{
		{
			name: "hardware acceleration disabled",
			job: func() TrickplayExtraction {
				job := trickplayJob(qsvOptions())
				job.AllowHardwareAcceleration = false
				return job
			},
			caps: intelMjpegCapabilities(),
			want: softwareTrickplayArgs(),
		},
		{
			name: "no hardware type",
			job:  func() TrickplayExtraction { return trickplayJob(HardwareOptions{}) },
			caps: intelMjpegCapabilities(),
			want: softwareTrickplayArgs(),
		},
		{
			name: "qsv decode and mjpeg_qsv",
			job:  func() TrickplayExtraction { return trickplayJob(qsvOptions()) },
			caps: intelMjpegCapabilities(),
			want: "-hide_banner -loglevel error -nostdin -init_hw_device vaapi=va:,vendor_id=0x8086,driver=iHD -init_hw_device qsv=qs@va -filter_hw_device qs " +
				"-hwaccel vaapi -hwaccel_output_format vaapi -noautorotate -i /media/movies/a.mkv -an -sn -vf " + trickplayTiming + "," + trickplayBT709 +
				",scale_vaapi=w=320:h=180:format=nv12:out_range=pc:mode=hq:extra_hw_frames=24,hwmap=derive_device=qsv,format=qsv" +
				" -threads 1 -c:v mjpeg_qsv -global_quality:v 91 " + trickplayImageOut,
		},
		{
			name: "qsv decode with hardware encoding off keeps mjpeg",
			job: func() TrickplayExtraction {
				job := trickplayJob(qsvOptions())
				job.EnableHardwareEncoding = false
				return job
			},
			caps: intelMjpegCapabilities(),
			want: "-c:v mjpeg -q:v 4",
		},
		{
			name: "encoding.xml hardware encoding off keeps mjpeg",
			job:  func() TrickplayExtraction { return trickplayJob(qsvNoEncoding) },
			caps: intelMjpegCapabilities(),
			want: "-c:v mjpeg -q:v 4",
		},
		{
			name: "vaapi decode and mjpeg_vaapi",
			job:  func() TrickplayExtraction { return trickplayJob(vaapi) },
			caps: intelMjpegCapabilities(),
			want: "-hide_banner -loglevel error -nostdin -init_hw_device vaapi=va:/dev/dri/renderD128,driver=iHD -hwaccel vaapi -hwaccel_output_format vaapi -noautorotate " +
				"-i /media/movies/a.mkv -an -sn -vf " + trickplayTiming + "," + trickplayBT709 +
				",scale_vaapi=w=320:h=180:format=nv12:out_range=pc:mode=hq:extra_hw_frames=24" +
				" -threads 1 -c:v mjpeg_vaapi -global_quality:v 91 " + trickplayImageOut,
		},
		{
			name: "nvdec with the software mjpeg encoder",
			job:  func() TrickplayExtraction { return trickplayJob(nvencOptions()) },
			caps: nvidiaCapabilities(),
			want: "-hide_banner -loglevel error -nostdin " + cudaDevice + " " + nvdecInput +
				" -i /media/movies/a.mkv -an -sn -vf " + trickplayTiming + "," + trickplayBT709 +
				",scale_cuda=w=320:h=180:format=yuv420p,hwdownload,format=yuv420p" +
				" -threads 1 -strict unofficial -c:v mjpeg -q:v 4 " + trickplayImageOut,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args, err := BuildTrickplayArgs(test.job(), test.caps)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(args, " ")
			if strings.HasPrefix(test.want, "-hide_banner") && got != test.want {
				t.Fatalf("args\n got %s\nwant %s", got, test.want)
			}
			if !strings.Contains(got, test.want) {
				t.Fatalf("args %s do not contain %s", got, test.want)
			}
		})
	}
}

func TestTrickplayDisplayStreamCorrectsStretchedHeight(t *testing.T) {
	stream := trickplayDisplayStream(VideoStream{Width: 720, Height: 480}, "16:9")
	if stream.Height != 405 {
		t.Fatalf("height = %d, want 405", stream.Height)
	}
	if got := trickplayDisplayStream(VideoStream{Width: 1920, Height: 1080}, "16:9"); got.Height != 1080 {
		t.Fatalf("height = %d, want 1080", got.Height)
	}
}

func TestBuildTrickplayArgsRejectsIncompleteJob(t *testing.T) {
	job := trickplayJob(HardwareOptions{})
	job.IntervalMilliseconds = 0
	if _, err := BuildTrickplayArgs(job, Capabilities{}); err == nil {
		t.Fatal("zero interval accepted")
	}
}
