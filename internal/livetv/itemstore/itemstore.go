// SPDX-License-Identifier: GPL-2.0-only

// Package itemstore adapts store.ItemStore to livetv.Items. It is separate
// from package livetv so that the guide logic builds without the store's
// image dependencies.
package itemstore

import (
	"context"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv"
	"github.com/deadfin-org/deadfin/internal/store"
)

// Items is livetv.Items over a store.ItemStore.
type Items struct {
	store *store.ItemStore
}

var _ livetv.Items = Items{}

// New returns the adapter.
func New(items *store.ItemStore) Items { return Items{store: items} }

// Item implements livetv.Items.
func (i Items) Item(ctx context.Context, id dto.UUID) (item.Item, bool, error) {
	values, err := i.store.GetBatch(ctx, []dto.UUID{id})
	if err != nil || len(values) == 0 {
		return item.Item{}, false, err
	}
	return values[0], true, nil
}

// Upsert implements livetv.Items.
func (i Items) Upsert(ctx context.Context, value item.Item) error {
	return i.store.Upsert(ctx, value)
}

// Children implements livetv.Items. It reads on the reconciliation
// connection, so the read cannot conflict with the write that follows.
func (i Items) Children(ctx context.Context, parentID dto.UUID) ([]item.Item, error) {
	return i.store.ReconciliationChildren(ctx, parentID)
}

// ReplaceChildren implements livetv.Items.
func (i Items) ReplaceChildren(ctx context.Context, parentID dto.UUID, desired []item.Item) (livetv.Changes, error) {
	result, err := i.store.ReconcileChildren(ctx, parentID, desired)
	if err != nil {
		return livetv.Changes{}, err
	}
	return livetv.Changes{Inserted: len(result.Inserted), Updated: len(result.Updated), Deleted: len(result.Deleted)}, nil
}

// MergeChildren implements livetv.Items.
func (i Items) MergeChildren(ctx context.Context, parentID dto.UUID, desired []item.Item) (livetv.Changes, error) {
	result, err := i.store.ReconcileScannedPlanAtWithOptions(ctx, parentID, desired, nil, nil, time.Time{}, store.ReconcileOptions{PreserveUnplannedChildren: true})
	if err != nil {
		return livetv.Changes{}, err
	}
	return livetv.Changes{Inserted: len(result.Inserted), Updated: len(result.Updated), Deleted: len(result.Deleted)}, nil
}
