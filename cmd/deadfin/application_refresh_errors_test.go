// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/deadfin-org/deadfin/internal/library"
)

// TestFoldRefreshErrorsIgnoresSkippedItems proves that a skipped item does
// not fail the scan or metadata-refresh task, and a folder failure does
// (deadfin#805). Upstream logs a per-item failure and completes the task.
func TestFoldRefreshErrorsIgnoresSkippedItems(t *testing.T) {
	skipped := fmt.Errorf("%w: skip item /c/Men in Black Collection [boxset]", library.ErrItemSkipped)
	folder := errors.New("resolve source /movies: permission denied")
	if err := foldRefreshErrors(nil, []error{skipped}); err != nil {
		t.Fatalf("fold skipped only = %v, want nil", err)
	}
	err := foldRefreshErrors(nil, []error{skipped, folder})
	if !errors.Is(err, folder) || errors.Is(err, library.ErrItemSkipped) {
		t.Fatalf("fold skipped and folder = %v, want the folder error only", err)
	}
	top := errors.New("canceled")
	if err := foldRefreshErrors(top, []error{skipped}); !errors.Is(err, top) {
		t.Fatalf("fold top-level = %v, want %v", err, top)
	}
}
