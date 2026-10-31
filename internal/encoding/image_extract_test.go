// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"reflect"
	"testing"
	"time"
)

func TestImageExtractionArgs(t *testing.T) {
	offset := 83*time.Minute + 4*time.Second + 250*time.Millisecond
	index := 3
	tests := []struct {
		name string
		job  ImageExtraction
		want []string
	}{
		{
			name: "screen grab I-frame pass",
			job:  ImageExtraction{InputPath: "/dev/fd/3", Container: "mkv", OutputPath: "/out.jpg", Offset: &offset, UseIFrame: true, EncoderAtLeast51: true},
			want: []string{"-nostdin", "-f", "matroska", "-ss", "01:23:04.250", "-i", "/dev/fd/3", "-threads", "0", "-v", "quiet", "-vframes", "1",
				"-vf", "scale=round(iw*sar/2)*2:round(ih/2)*2,thumbnail=n=24", "-fps_mode", "auto", "-f", "image2", "/out.jpg"},
		},
		{
			name: "standard pass with resolution, threads, deinterlace and tonemapx",
			job: ImageExtraction{InputPath: "/in.ts", Container: "ts", OutputPath: "/out.jpg", Offset: &offset, Interlaced: true, HDR: true, DolbyVision: true,
				SupportsTonemapX: true, Resolution: "1280x720", Threads: 4},
			want: []string{"-nostdin", "-f", "mpegts", "-ss", "01:23:04.250", "-i", "/in.ts", "-threads", "4", "-v", "quiet", "-vframes", "1",
				"-vf", "bwdif=0:-1:0,scale=round(iw*sar/2)*2:round(ih/2)*2,tonemapx=tonemap=bt2390:desat=0:peak=400:t=bt709:m=bt709:p=bt709:format=yuv420p:range=full",
				"-s", "1280x720", "-vsync", "-1", "-f", "image2", "/out.jpg"},
		},
		{
			name: "embedded image stream with perf tradeoff",
			job:  ImageExtraction{InputPath: "/in.mp4", Container: "mov,mp4,m4a", OutputPath: "/out.png", StreamIndex: &index, UseIFrame: true, PerfTradeoff: true, EncoderAtLeast51: true},
			want: []string{"-nostdin", "-f", "mov,mp4,m4a", "-skip_frame", "nokey", "-i", "/in.mp4", "-map", "0:3", "-threads", "0", "-v", "quiet", "-vframes", "1",
				"-vf", "scale=round(iw*sar/2)*2:round(ih/2)*2", "-fps_mode", "auto", "-f", "image2", "/out.png"},
		},
		{
			name: "audio image omits the resolution",
			job:  ImageExtraction{InputPath: "/dev/fd/3", OutputPath: "/out.jpg", StreamIndex: &index, Audio: true, Resolution: "1280x720", EncoderAtLeast51: true},
			want: []string{"-nostdin", "-i", "/dev/fd/3", "-map", "0:3", "-threads", "0", "-v", "quiet", "-vframes", "1",
				"-vf", "scale=round(iw*sar/2)*2:round(ih/2)*2", "-fps_mode", "auto", "-f", "image2", "/out.jpg"},
		},
		{
			name: "zscale does not tonemap Dolby Vision",
			job:  ImageExtraction{InputPath: "/in.m2ts", Container: "m2ts", OutputPath: "/out.jpg", HDR: true, DolbyVision: true, SupportsZscale: true},
			want: []string{"-nostdin", "-i", "/in.m2ts", "-threads", "0", "-v", "quiet", "-vframes", "1",
				"-vf", "scale=round(iw*sar/2)*2:round(ih/2)*2", "-vsync", "-1", "-f", "image2", "/out.jpg"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ImageExtractionArgs(test.job); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("args =\n%q\nwant\n%q", got, test.want)
			}
		})
	}
}

func TestImageExtractionHDRTonemap(t *testing.T) {
	if (ImageExtraction{HDR: true, SupportsZscale: true}).HDRTonemap() != true {
		t.Fatal("zscale HDR extraction is not a tonemap")
	}
	if (ImageExtraction{HDR: true, DolbyVision: true, SupportsZscale: true}).HDRTonemap() {
		t.Fatal("zscale Dolby Vision extraction is a tonemap")
	}
	if (ImageExtraction{SupportsTonemapX: true}).HDRTonemap() {
		t.Fatal("SDR extraction is a tonemap")
	}
}
