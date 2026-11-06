-- +goose Up
-- The guide grid and the channels' current programmes read the programmes of
-- a list of channels under the Live TV folder that overlap a window. With
-- top_parent_id and kind as equality columns before channel_id, the planner
-- prefers this index to ix_items_kind_top and ix_items_top_kind_start
-- without table statistics, and a lookup reads only the listed channels.
CREATE INDEX ix_items_top_kind_channel_start ON items(top_parent_id, kind, channel_id, start_date) WHERE channel_id IS NOT NULL;

-- +goose Down
DROP INDEX ix_items_top_kind_channel_start;
