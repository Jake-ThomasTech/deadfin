// SPDX-License-Identifier: GPL-2.0-only

//go:build cgo

package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/images"
	"github.com/deadfin-org/deadfin/internal/imageserve"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/path"
)

const stockCorpusRoot = "../../images/testdata/stock-corpus"

const minimumServingSSIM = 0.98

type corpusManifest struct {
	Cases []corpusCase `json:"cases"`
}

type corpusCase struct {
	ID       string `json:"id"`
	Expected struct {
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		MimeType string `json:"mimeType"`
	} `json:"expected"`
	Request struct {
		Capture struct {
			Path       string `json:"path"`
			Method     string `json:"method"`
			ImageType  string `json:"imageType"`
			HTTPStatus int    `json:"httpStatus"`
		} `json:"capture"`
		OutputFormat        string `json:"outputFormat"`
		Quality             int    `json:"quality"`
		SourceModifiedUTC   string `json:"sourceModifiedUTC"`
		AutoOrient          bool   `json:"autoOrient"`
		ProcessorAutoOrient *bool  `json:"processorAutoOrient"`
	} `json:"request"`
	Source struct {
		Kind string `json:"kind"`
		Path string `json:"path"`
	} `json:"source"`
	Stock struct {
		Path string `json:"path"`
	} `json:"stock"`
}

type corpusDownloader struct{ body []byte }

func (d corpusDownloader) Fetch(_ context.Context, _ string) ([]byte, error) {
	return d.body, nil
}

// TestImageServingAcceptance drives the real libvips processor through the HTTP
// image routes. It owns the single process-lifetime image runtime and shares one
// processor across every subtest, mirroring internal/images' TestNativeProcessor.
// libvips shutdown is terminal for a process, so each run of the test opens the
// runtime in a fresh child process; -count=N runs N children (#666).
func TestImageServingAcceptance(t *testing.T) {
	const childEnvironment = "DEADFIN_TEST_IMAGE_SERVING_CHILD"
	if os.Getenv(childEnvironment) != "1" {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestImageServingAcceptance$", "-test.v", "-test.count=1")
		command.Env = append(os.Environ(), childEnvironment+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("image serving child: %v\n%s", err, output)
		}
		t.Logf("image serving child:\n%s", output)
		return
	}
	imageRuntime, err := images.OpenRuntime(context.Background())
	if err != nil {
		t.Fatalf("open image runtime: %v", err)
	}
	defer func() { _ = imageRuntime.Close() }()
	processor, err := images.NewProcessor(imageRuntime, images.ProcessorConfig{CacheRoot: t.TempDir(), MaxConcurrent: 2})
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}

	manifest := loadCorpusManifest(t)

	t.Run("stock-corpus-over-http", func(t *testing.T) {
		service := imageserve.New(processor, nil)
		for _, testCase := range manifest.Cases {
			testCase := testCase
			if testCase.Source.Kind != "local-fixture" || testCase.Request.Capture.Path == "" {
				continue
			}
			t.Run(testCase.ID, func(t *testing.T) {
				if testCase.Request.OutputFormat == "webp" && testCase.Request.Quality == 0 && runtime.GOOS != "linux" {
					t.Skip("SkiaSharp WebP quality-zero requires the packaged Linux runtime")
				}
				if effectiveAutoOrient(testCase) {
					t.Skip("item-orientation-resolved rotation is not wired to the serving route yet; validated at the processor level (internal/images corpus)")
				}
				info := corpusImageInfo(t, testCase)
				rt := router.New()
				handlers.RegisterImages(rt, fakeImageReader{infos: []item.ImageInfo{info}}, service, path.VirtualPathExpander{})

				recorder := httptest.NewRecorder()
				rt.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.Request.Capture.Path, nil))
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
				}
				if got := recorder.Header().Get("Content-Type"); got != testCase.Expected.MimeType {
					t.Errorf("Content-Type = %q, want %q", got, testCase.Expected.MimeType)
				}
				rendered := decodeImage(t, recorder.Body.Bytes())
				assertDimensions(t, rendered, testCase.Expected.Width, testCase.Expected.Height)
				assertSSIMAgainstStock(t, rendered, filepath.Join(stockCorpusRoot, testCase.Stock.Path))
			})
		}
	})

	t.Run("etag-and-conditional-get", func(t *testing.T) {
		testCase := findCase(t, manifest, "jpeg-landscape-width-320")
		info := corpusImageInfo(t, testCase)
		rt := router.New()
		handlers.RegisterImages(rt, fakeImageReader{infos: []item.ImageInfo{info}}, imageserve.New(processor, nil), path.VirtualPathExpander{})

		recorder := httptest.NewRecorder()
		rt.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.Request.Capture.Path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("initial status = %d, want 200", recorder.Code)
		}
		etag := recorder.Header().Get("ETag")
		if etag != `"`+filepath.Base(info.Path)+`"` {
			t.Fatalf("ETag = %q, want the source base name quoted", etag)
		}

		conditional := httptest.NewRequest(http.MethodGet, testCase.Request.Capture.Path, nil)
		conditional.Header.Set("If-None-Match", etag)
		conditionalRecorder := httptest.NewRecorder()
		rt.ServeHTTP(conditionalRecorder, conditional)
		if conditionalRecorder.Code != http.StatusNotModified {
			t.Fatalf("conditional status = %d, want 304", conditionalRecorder.Code)
		}
		if conditionalRecorder.Body.Len() != 0 {
			t.Errorf("304 body length = %d, want 0", conditionalRecorder.Body.Len())
		}
	})

	t.Run("remote-download-and-serve", func(t *testing.T) {
		testCase := findCase(t, manifest, "jpeg-landscape-width-320")
		fixture, err := os.ReadFile(filepath.Join(stockCorpusRoot, testCase.Source.Path))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		remoteCache := imageserve.NewRemoteCache(corpusDownloader{body: fixture}, t.TempDir())
		service := imageserve.New(processor, remoteCache)

		// A remote provider URL in ImageInfo.Path (no local file) must be
		// downloaded into the cache, then served resized — the poster path for
		// NFO-less items whose artwork P8 stored as a TMDb CDN URL.
		const remoteURL = "https://image.tmdb.org/t/p/original/poster.jpg"
		reader := fakeImageReader{infos: []item.ImageInfo{{Type: item.ImageTypePrimary, Index: 0, Path: remoteURL}}}
		rt := router.New()
		handlers.RegisterImages(rt, reader, service, path.VirtualPathExpander{})

		recorder := httptest.NewRecorder()
		rt.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/Items/"+testItemHex+"/Images/Primary?width=320&quality=90&format=jpeg", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); got != "image/jpeg" {
			t.Errorf("Content-Type = %q, want image/jpeg", got)
		}
		if got := recorder.Header().Get("ETag"); got != `"poster.jpg"` {
			t.Errorf("ETag = %q, want the remote URL base name quoted", got)
		}
		rendered := decodeImage(t, recorder.Body.Bytes())
		assertDimensions(t, rendered, testCase.Expected.Width, testCase.Expected.Height)
		assertSSIMAgainstStock(t, rendered, filepath.Join(stockCorpusRoot, testCase.Stock.Path))
	})
}

func loadCorpusManifest(t *testing.T) corpusManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stockCorpusRoot, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest corpusManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Cases) == 0 {
		t.Fatal("manifest has no cases")
	}
	return manifest
}

// effectiveAutoOrient mirrors the corpus's item-orientation resolution: the
// processor-resolved override wins when present, else the raw request intent.
func effectiveAutoOrient(testCase corpusCase) bool {
	if testCase.Request.ProcessorAutoOrient != nil {
		return *testCase.Request.ProcessorAutoOrient
	}
	return testCase.Request.AutoOrient
}

func findCase(t *testing.T, manifest corpusManifest, id string) corpusCase {
	t.Helper()
	for _, testCase := range manifest.Cases {
		if testCase.ID == id {
			return testCase
		}
	}
	t.Fatalf("manifest lacks case %q", id)
	return corpusCase{}
}

func corpusImageInfo(t *testing.T, testCase corpusCase) item.ImageInfo {
	t.Helper()
	sourcePath, err := filepath.Abs(filepath.Join(stockCorpusRoot, testCase.Source.Path))
	if err != nil {
		t.Fatalf("resolve source path: %v", err)
	}
	imageType := item.ImageTypePrimary
	if name := testCase.Request.Capture.ImageType; name != "" && name != "Primary" {
		t.Fatalf("unexpected capture image type %q (only Primary captures are covered)", name)
	}
	modified, err := time.Parse(time.RFC3339, testCase.Request.SourceModifiedUTC)
	if err != nil {
		t.Fatalf("parse sourceModifiedUTC %q: %v", testCase.Request.SourceModifiedUTC, err)
	}
	return item.ImageInfo{Type: imageType, Index: 0, Path: sourcePath, DateModified: &modified}
}

func decodeImage(t *testing.T, body []byte) image.Image {
	t.Helper()
	decoded, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode rendered image: %v", err)
	}
	return decoded
}

func assertDimensions(t *testing.T, img image.Image, width, height int) {
	t.Helper()
	bounds := img.Bounds()
	if bounds.Dx() != width || bounds.Dy() != height {
		t.Errorf("dimensions = %dx%d, want %dx%d", bounds.Dx(), bounds.Dy(), width, height)
	}
}

func assertSSIMAgainstStock(t *testing.T, rendered image.Image, stockPath string) {
	t.Helper()
	stockBytes, err := os.ReadFile(stockPath)
	if err != nil {
		t.Fatalf("read stock %q: %v", stockPath, err)
	}
	stock, _, err := image.Decode(bytes.NewReader(stockBytes))
	if err != nil {
		t.Fatalf("decode stock %q: %v", stockPath, err)
	}
	score, err := images.SSIM(rendered, stock)
	if err != nil {
		t.Fatalf("SSIM: %v", err)
	}
	if score < minimumServingSSIM {
		t.Errorf("SSIM = %f, want >= %f", score, minimumServingSSIM)
	}
}
