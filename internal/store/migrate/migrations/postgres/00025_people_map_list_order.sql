-- +goose Up
-- list_order is the credit's position in the item's people list (upstream
-- PeopleBaseItemMap.ListOrder). It orders credits that share a sort_order, so
-- NFO cast without <sortorder> keeps the file order. Rows written before this
-- migration keep 0 until the item is saved again. The version is 00025 because
-- the v0.4 and train/transcode-workers lines hold 00022 to 00024.
ALTER TABLE people_map ADD COLUMN list_order INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE people_map DROP COLUMN list_order;
