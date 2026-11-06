-- +goose Up
-- The PostgreSQL variant rebuilds the start indexes with start_date NULLS
-- FIRST to match the ascending item order. SQLite already sorts NULL values
-- first in an ascending index, so its indexes stay as they are.
SELECT 1;

-- +goose Down
SELECT 1;
