-- +goose Up
-- Live TV programmes are items under the Live TV folder. The guide grid
-- reads the programmes of the folder that overlap a window in start order
-- (upstream BaseItemConfiguration index Type, TopParentId, StartDate). The
-- index leads with top_parent_id so that a lookup by kind alone keeps
-- ix_items_kind_top. A channel's programmes are read by channel and start
-- (ChannelIds queries and the guide refresh).
CREATE INDEX ix_items_top_kind_start ON items(top_parent_id, kind, start_date);
CREATE INDEX ix_items_channel_start ON items(channel_id, start_date) WHERE channel_id IS NOT NULL;

-- +goose Down
DROP INDEX ix_items_channel_start;
DROP INDEX ix_items_top_kind_start;
