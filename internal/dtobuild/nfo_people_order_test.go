// SPDX-License-Identifier: GPL-2.0-only

package dtobuild_test

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/store"
)

// TestBuilderOrdersNFOPeopleAsUpstream covers issue #793 with the deadfin-web
// acceptance NFO. Upstream BaseNfoParser gives a <director> no SortOrder and
// an <actor> a SortOrder only from <order> or <sortorder>. AttachPeople then
// puts actors first, and PeopleBaseItemMap.ListOrder keeps the file order of
// actors that share a SortOrder.
func TestBuilderOrdersNFOPeopleAsUpstream(t *testing.T) {
	const document = `<movie><title>Pressure Hull</title>
  <director>Ada Marsh</director>
  <actor><name>Ivo Trench</name><role>Captain</role><type>Actor</type></actor>
  <actor><name>Mira Deep</name><role>Engineer</role><type>Actor</type></actor>
  <actor><name>Bo Keel</name><role>Pilot</role><type>Actor</type></actor>
  <actor><name>Cy Fathom</name><role>Cook</role><type>Actor</type></actor>
</movie>`
	provider := providers.NewNFOProvider(providers.NFOProviderOptions{
		Open: func(context.Context, string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(document)), nil
		},
	})
	refresh := library.RefreshContext{Directory: library.Entry{Path: "/media", IsDir: true}, Entries: []library.Entry{{Path: "/media/movie.mkv"}, {Path: "/media/movie.nfo"}}}
	path := "/media/movie.mkv"
	movie := item.Item{ID: builderUUID(t, 23), Kind: item.KindMovie, Path: &path}
	if _, err := provider.Refresh(t.Context(), refresh, &movie); err != nil {
		t.Fatalf("refresh NFO: %v", err)
	}
	items := openBuilderItemStore(t)
	if err := items.Upsert(t.Context(), movie); err != nil {
		t.Fatalf("upsert movie: %v", err)
	}
	loaded, err := items.Get(t.Context(), movie.ID)
	if err != nil {
		t.Fatalf("load movie: %v", err)
	}
	builder := dtobuild.NewBuilder(items, dtobuild.NewStoreEnricher(items, nil, nil))
	result, err := builder.Build(t.Context(), []item.Item{loaded}, store.ItemQueryDTOOptions{Fields: []string{"People"}}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got []string
	for _, person := range result.Items[0].People {
		got = append(got, person.Name+"/"+person.Type)
	}
	want := []string{"Ivo Trench/Actor", "Mira Deep/Actor", "Bo Keel/Actor", "Cy Fathom/Actor", "Ada Marsh/Director"}
	if !slices.Equal(got, want) {
		t.Fatalf("People = %v, want %v", got, want)
	}
}
