// SPDX-License-Identifier: GPL-2.0-only

package dtobuild_test

import (
	"slices"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/store"
)

// TestBuilderProjectsPersonPrimaryImageTags covers DtoService.AttachPeople:
// each credit carries its Person entity's Primary image tag and BlurHash, so
// clients request cast photos. A person without a Primary image has no tag.
func TestBuilderProjectsPersonPrimaryImageTags(t *testing.T) {
	items := openBuilderItemStore(t)
	alice, bob := library.PersonEntity("Alice"), library.PersonEntity("Bob")
	hash := "LEHV6nWB2yk8"
	alice.ImageInfos = []item.ImageInfo{{Type: item.ImageTypePrimary, Path: "https://image.tmdb.org/t/p/original/alice.jpg", BlurHash: &hash}}
	name, first, second := "Movie", 0, 1
	movie := item.Item{ID: builderUUID(t, 21), Kind: item.KindMovie, Name: &name, People: []item.PersonCredit{
		{PersonID: alice.ID, Name: "Alice", Type: item.PersonKindActor, SortOrder: &first},
		{PersonID: bob.ID, Name: "Bob", Type: item.PersonKindDirector, SortOrder: &second},
	}}
	for _, value := range []item.Item{alice, bob, movie} {
		if err := items.Upsert(t.Context(), value); err != nil {
			t.Fatalf("upsert %s: %v", value.ID, err)
		}
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
	wire := builderWireObject(t, result.Items[0])
	got := string(wire["People"])
	wantPeople := `[{"Id":"` + alice.ID.String() + `","Name":"Alice","Role":"","Type":"Actor","PrimaryImageTag":"alice.jpg","ImageBlurHashes":{"Primary":{"alice.jpg":"LEHV6nWB2yk8"}}},{"Id":"` + bob.ID.String() + `","Name":"Bob","Role":"","Type":"Director"}]`
	if got != wantPeople {
		t.Fatalf("People = %s, want %s", got, wantPeople)
	}
}

// TestBuilderOrdersPeopleActorsFirst covers DtoService.AttachPeople ordering:
// credits sort by SortOrder (nil last), then actors before guest stars,
// directors, writers, and producers. Issue #793: a Director with no
// SortOrder came before the Actor.
func TestBuilderOrdersPeopleActorsFirst(t *testing.T) {
	items := openBuilderItemStore(t)
	director, writer, guest, actor, ranked := library.PersonEntity("Ada Marsh"), library.PersonEntity("Wren Hale"), library.PersonEntity("Gus Pell"), library.PersonEntity("Ivo Trench"), library.PersonEntity("Rae Lund")
	name, first := "Movie", 0
	movie := item.Item{ID: builderUUID(t, 22), Kind: item.KindMovie, Name: &name, People: []item.PersonCredit{
		{PersonID: director.ID, Name: "Ada Marsh", Type: item.PersonKindDirector},
		{PersonID: writer.ID, Name: "Wren Hale", Type: item.PersonKindWriter},
		{PersonID: guest.ID, Name: "Gus Pell", Type: item.PersonKindGuestStar},
		{PersonID: actor.ID, Name: "Ivo Trench", Type: item.PersonKindActor},
		{PersonID: ranked.ID, Name: "Rae Lund", Type: item.PersonKindProducer, SortOrder: &first},
	}}
	for _, value := range []item.Item{director, writer, guest, actor, ranked, movie} {
		if err := items.Upsert(t.Context(), value); err != nil {
			t.Fatalf("upsert %s: %v", value.ID, err)
		}
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
	want := []string{"Rae Lund/Producer", "Ivo Trench/Actor", "Gus Pell/GuestStar", "Ada Marsh/Director", "Wren Hale/Writer"}
	if !slices.Equal(got, want) {
		t.Fatalf("People = %v, want %v", got, want)
	}
}
