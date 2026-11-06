-- +goose Up
-- A search matches clean_name and original_title with a contains pattern, so
-- no index can seek it and the filter visits every item. This index holds
-- every column the search filter reads, so the visit reads the index, not
-- the wide item rows (GET /Search/Hints, deadfin#826).
CREATE INDEX ix_items_search ON items(clean_name, original_title, kind, is_virtual_item, id);
-- Statistics for the new index let the planner weigh it against a scan of
-- the table.
ANALYZE items;

-- +goose Down
DROP INDEX ix_items_search;
