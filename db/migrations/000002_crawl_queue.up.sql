-- Crawler scheduling queue.
-- Uses FOR UPDATE SKIP LOCKED for concurrent-safe worker claiming (no Redis needed).

CREATE TABLE crawl_targets (
    player_tag                  VARCHAR(16) PRIMARY KEY REFERENCES players(tag),
    priority                    SMALLINT NOT NULL DEFAULT 5,       -- 1 = highest, 10 = lowest
    next_crawl_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_crawled_at             TIMESTAMPTZ,
    last_crawl_status           VARCHAR(16),                        -- 'success','not_found','rate_limited','error'
    consecutive_failures        INTEGER NOT NULL DEFAULT 0,
    discovery_source            VARCHAR(32) NOT NULL,               -- 'seed','battle_discovery','leaderboard','manual'
    discovery_via_player        VARCHAR(16),                        -- tag that led to this discovery
    trophy_estimate             INTEGER,                            -- trophies at time of discovery
    trophy_bucket_at_discovery  SMALLINT,
    crawl_count                 INTEGER NOT NULL DEFAULT 0,
    is_active                   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Primary scheduling index: active targets due for crawling, ordered by priority then time.
-- SKIP LOCKED workers will claim rows from this index.
CREATE INDEX idx_crawl_targets_schedule
    ON crawl_targets(next_crawl_at, priority)
    WHERE is_active = TRUE;

CREATE INDEX idx_crawl_targets_discovery_source ON crawl_targets(discovery_source);
CREATE INDEX idx_crawl_targets_trophy_bucket    ON crawl_targets(trophy_bucket_at_discovery);
