-- +goose Up
-- Item queries order an ascending date NULLS FIRST and a descending date
-- NULLS LAST, as the captured SQLite source does. A PostgreSQL B-tree is
-- NULLS LAST by default, so an ascending start order did not match the start
-- indexes and the planner sorted every programme before it applied LIMIT.
-- With start_date NULLS FIRST the index matches the ascending order forward
-- and the descending order backward.
DROP INDEX ix_items_top_kind_start;
CREATE INDEX ix_items_top_kind_start ON items(top_parent_id, kind, start_date NULLS FIRST);
DROP INDEX ix_items_channel_start;
CREATE INDEX ix_items_channel_start ON items(channel_id, start_date NULLS FIRST) WHERE channel_id IS NOT NULL;
DROP INDEX ix_items_top_kind_channel_start;
CREATE INDEX ix_items_top_kind_channel_start ON items(top_parent_id, kind, channel_id, start_date NULLS FIRST) WHERE channel_id IS NOT NULL;

-- +goose Down
DROP INDEX ix_items_top_kind_channel_start;
CREATE INDEX ix_items_top_kind_channel_start ON items(top_parent_id, kind, channel_id, start_date) WHERE channel_id IS NOT NULL;
DROP INDEX ix_items_channel_start;
CREATE INDEX ix_items_channel_start ON items(channel_id, start_date) WHERE channel_id IS NOT NULL;
DROP INDEX ix_items_top_kind_start;
CREATE INDEX ix_items_top_kind_start ON items(top_parent_id, kind, start_date);
