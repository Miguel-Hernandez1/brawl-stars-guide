ALTER TABLE crawl_targets
    DROP COLUMN IF EXISTS crawl_generation,
    DROP COLUMN IF EXISTS leased_until;
