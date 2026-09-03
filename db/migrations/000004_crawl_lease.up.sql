-- Add lease columns to crawl_targets for the M2 concurrent crawler.
-- crawl_generation: monotonically incremented at each claim; used for compare-and-update
--   in the finalize step to detect stale workers.
-- leased_until: expiry of the current in-progress lease (NULL when idle).
--   The claim query excludes rows where leased_until > NOW().
ALTER TABLE crawl_targets
    ADD COLUMN crawl_generation INTEGER     NOT NULL DEFAULT 0,
    ADD COLUMN leased_until      TIMESTAMPTZ;
