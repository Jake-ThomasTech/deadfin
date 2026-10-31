// SPDX-License-Identifier: GPL-2.0-only

package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
)

// Default single-image extraction timeouts (MediaEncoder.cs
// DefaultSdrImageExtractionTimeout, DefaultHdrImageExtractionTimeout).
const (
	DefaultSDRImageExtractionTimeout = 10 * time.Second
	DefaultHDRImageExtractionTimeout = 20 * time.Second
)

// ImageExtractor runs single-frame image extractions for the Screen Grabber
// and the Embedded Image Extractor (MediaEncoder.ExtractImage). It bounds
// concurrent jobs like upstream's _thumbnailResourcePool.
type ImageExtractor struct {
	extractor Extractor
	slots     chan struct{}
}

// NewImageExtractor returns an extractor for the ffmpeg at path that runs at
// most parallel jobs at once. A parallel value below one is one.
func NewImageExtractor(path string, parallel int) *ImageExtractor {
	return &ImageExtractor{extractor: Extractor{Path: path}, slots: make(chan struct{}, max(1, parallel))}
}

// Extract writes one image from source to job.OutputPath. It tries the
// I-frame extraction first and then the standard extraction, as
// MediaEncoder.ExtractImage does. An audio image takes the standard
// extraction only. job.InputPath and job.UseIFrame are set here. A timeout of zero or less selects the upstream default.
func (e *ImageExtractor) Extract(ctx context.Context, job encoding.ImageExtraction, source *sourcefile.Source, timeout time.Duration) error {
	if e == nil {
		return errors.New("ffmpeg image extractor is required")
	}
	if source == nil {
		return errors.New("ffmpeg image extraction source is required")
	}
	if timeout <= 0 {
		timeout = DefaultSDRImageExtractionTimeout
		if job.HDRTonemap() {
			timeout = DefaultHDRImageExtractionTimeout
		}
	}
	job.InputPath = sourcefile.FDPath(ExtractorSourceFD)
	if job.Audio {
		job.UseIFrame = false
		return e.extractOnce(ctx, job, source, timeout)
	}
	job.UseIFrame = true
	firstErr := e.extractOnce(ctx, job, source, timeout)
	if firstErr == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	job.UseIFrame = false
	if err := e.extractOnce(ctx, job, source, timeout); err != nil {
		return fmt.Errorf("ffmpeg image extraction failed: %w (I-frame attempt: %v)", err, firstErr)
	}
	return nil
}

func (e *ImageExtractor) extractOnce(ctx context.Context, job encoding.ImageExtraction, source *sourcefile.Source, timeout time.Duration) error {
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	_ = os.Remove(job.OutputPath)
	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := e.extractor.Run(jobCtx, encoding.ImageExtractionArgs(job), source)
	if err != nil {
		_ = os.Remove(job.OutputPath)
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("ffmpeg image extraction timed out after %s", timeout)
		}
		return err
	}
	info, err := os.Stat(job.OutputPath)
	if err != nil || info.Size() == 0 {
		_ = os.Remove(job.OutputPath)
		return errors.New("ffmpeg image extraction wrote no image")
	}
	return nil
}
