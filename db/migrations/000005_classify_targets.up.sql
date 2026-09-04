-- Rename trophy_bucket_at_discovery to player_trophy_bucket.
-- The old name implied the bucket is set at discovery time; in practice it is assigned
-- only after the first successful GetPlayer call (when profile.Trophies is known).
-- The column is updated on every subsequent successful crawl, not just the first.
ALTER TABLE crawl_targets RENAME COLUMN trophy_bucket_at_discovery TO player_trophy_bucket;

DROP INDEX IF EXISTS idx_crawl_targets_trophy_bucket;
CREATE INDEX idx_crawl_targets_player_trophy_bucket ON crawl_targets(player_trophy_bucket);
