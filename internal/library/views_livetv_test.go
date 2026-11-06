// SPDX-License-Identifier: GPL-2.0-only

package library_test

import (
	"context"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
)

type liveTvFolderSource struct {
	folder item.Item
	ok     bool
}

func (s liveTvFolderSource) LiveTvFolder(context.Context) (item.Item, bool, error) {
	return s.folder, s.ok, nil
}

// TestViewsQueryAddsLiveTvFolder pins UserViewManager.cs:117-134: the Live
// TV folder joins the external views of a user with Live TV access while a
// tuner exists.
func TestViewsQueryAddsLiveTvFolder(t *testing.T) {
	name := "Live TV"
	viewType := item.CollectionTypeLiveTV
	folder := item.Item{ID: viewsID(t, 77), Kind: item.KindUserView, Name: &name, IsFolder: true, Extra: item.ExtraFields{UserView: &item.UserViewExtra{ViewType: &viewType}}}
	hasLiveTv := func(t *testing.T, views *library.Views, external bool, userID dto.UUID) bool {
		t.Helper()
		result, err := views.Query(t.Context(), library.UserViewsQuery{UserID: userID, IncludeExternalContent: external})
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range result.Items {
			if value.ID == folder.ID {
				return true
			}
		}
		return false
	}

	fixture := newPersistedViewsFixture(t)
	fixture.views.WithLiveTv(liveTvFolderSource{folder: folder, ok: true})
	if !hasLiveTv(t, fixture.views, true, fixture.user.ID) {
		t.Fatal("Live TV folder missing for a user with Live TV access")
	}
	if hasLiveTv(t, fixture.views, false, fixture.user.ID) {
		t.Fatal("Live TV folder present without IncludeExternalContent")
	}
	fixture.configure(t, map[string]any{"MyMediaExcludes": []dto.UUID{folder.ID}}, nil)
	if hasLiveTv(t, fixture.views, true, fixture.user.ID) {
		t.Fatal("Live TV folder present although the user hides it")
	}
	fixture.configure(t, map[string]any{}, map[string]any{"EnableLiveTvAccess": false})
	if hasLiveTv(t, fixture.views, true, fixture.user.ID) {
		t.Fatal("Live TV folder present for a user without Live TV access")
	}
	fixture.configure(t, map[string]any{}, map[string]any{"EnableLiveTvAccess": true})
	fixture.views.WithLiveTv(liveTvFolderSource{})
	if hasLiveTv(t, fixture.views, true, fixture.user.ID) {
		t.Fatal("Live TV folder present while no tuner exists")
	}
}
