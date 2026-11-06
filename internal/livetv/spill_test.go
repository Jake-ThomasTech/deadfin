// SPDX-License-Identifier: GPL-2.0-only

package livetv

import (
	"os"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/livetv/guide"
)

// A spill keeps each batch's programmes in the order they were added and
// does not carry fields from one record into the next.
func TestProgramSpillBatches(t *testing.T) {
	root := t.TempDir()
	// 3500 channel-days per batch at 1750 days: 2 channels per batch.
	spill, err := newProgramSpill(root, 5, 1750)
	if err != nil {
		t.Fatal(err)
	}
	if spill.batches() != 3 {
		t.Fatalf("batches %d", spill.batches())
	}
	if first, last := spill.bounds(2, 5); first != 4 || last != 5 {
		t.Fatalf("bounds %d-%d", first, last)
	}
	name := "named"
	start := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	added := []struct {
		index   int
		program guide.Program
	}{
		{0, guide.Program{ID: "a", Name: &name, StartDate: start}},
		{4, guide.Program{ID: "b", IsMovie: true}},
		{1, guide.Program{ID: "c"}},
		{0, guide.Program{ID: "d", Genres: []string{"News"}}},
	}
	for _, value := range added {
		if err := spill.add(value.index, value.program); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	err = spill.read(0, func(index int, program guide.Program) {
		got = append(got, program.ID)
		switch program.ID {
		case "a":
			if index != 0 || program.Name == nil || *program.Name != name || !program.StartDate.Equal(start) {
				t.Fatalf("record a: %d %+v", index, program)
			}
		case "c":
			if index != 1 || program.Name != nil || !program.StartDate.IsZero() {
				t.Fatalf("record c carries fields: %d %+v", index, program)
			}
		case "d":
			if index != 0 || program.Name != nil || len(program.Genres) != 1 {
				t.Fatalf("record d: %d %+v", index, program)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "c" || got[2] != "d" {
		t.Fatalf("batch 0 %v", got)
	}
	// A batch can be read again until it is released.
	count := 0
	if err := spill.read(0, func(int, guide.Program) { count++ }); err != nil || count != 3 {
		t.Fatalf("second read %d %v", count, err)
	}
	spill.release(0)
	if err := spill.read(1, func(int, guide.Program) { t.Fatal("batch 1 is empty") }); err != nil {
		t.Fatal(err)
	}
	if err := spill.read(2, func(index int, program guide.Program) {
		if index != 4 || !program.IsMovie {
			t.Fatalf("batch 2: %d %+v", index, program)
		}
	}); err != nil {
		t.Fatal(err)
	}

	spill.close()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left %v %v", entries, err)
	}
}
