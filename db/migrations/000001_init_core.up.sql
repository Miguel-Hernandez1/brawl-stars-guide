-- Core reference and data tables.
-- Aggregate tables (agg_*) are created in a later migration once the schema is stable.

-- ---------------------------------------------------------------------------
-- brawlers
-- Static reference table. Never deleted - retired brawlers get is_active = false.
-- Updated from /brawlers endpoint or manually after each patch.
-- ---------------------------------------------------------------------------
CREATE TABLE brawlers (
    id          INTEGER PRIMARY KEY,    -- Supercell's stable numeric ID
    name        VARCHAR(64) NOT NULL,
    rarity      VARCHAR(32),            -- 'common','rare','super_rare','epic','mythic','legendary'
    class       VARCHAR(32),            -- Supercell's brawler class/role
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    raw_data    JSONB,                  -- full /brawlers/{id} response for schema resilience
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- patches
-- Manually maintained patch registry. is_current = TRUE for exactly one row.
-- ---------------------------------------------------------------------------
CREATE TABLE patches (
    id          SERIAL PRIMARY KEY,
    version     VARCHAR(16) NOT NULL UNIQUE,
    released_at TIMESTAMPTZ NOT NULL,
    is_current  BOOLEAN NOT NULL DEFAULT FALSE,
    notes       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Only one patch can be current at a time.
CREATE UNIQUE INDEX idx_patches_one_current ON patches (is_current) WHERE is_current = TRUE;

-- ---------------------------------------------------------------------------
-- balance_changes
-- Per-brawler change records per patch. Powers the Updates product surface.
-- ---------------------------------------------------------------------------
CREATE TABLE balance_changes (
    id           SERIAL PRIMARY KEY,
    patch_id     INTEGER NOT NULL REFERENCES patches(id),
    brawler_id   INTEGER NOT NULL REFERENCES brawlers(id),
    change_type  VARCHAR(32) NOT NULL,  -- 'buff','nerf','rework','new','cosmetic'
    attribute    VARCHAR(64),           -- 'health','main_attack_damage', etc.
    value_before NUMERIC,
    value_after  NUMERIC,
    description  TEXT NOT NULL,         -- human-readable summary
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_balance_changes_patch_id    ON balance_changes(patch_id);
CREATE INDEX idx_balance_changes_brawler_id  ON balance_changes(brawler_id);

-- ---------------------------------------------------------------------------
-- events
-- Map/mode reference. Surrogate PK + unique constraint on (supercell_id, mode, map_name)
-- so that if Supercell ever reuses an event ID for a different map, we get a new row
-- rather than a collision.
-- ---------------------------------------------------------------------------
CREATE TABLE events (
    id                  SERIAL PRIMARY KEY,
    supercell_id        INTEGER NOT NULL,
    mode                VARCHAR(64) NOT NULL,
    map_name            VARCHAR(128) NOT NULL,
    is_ranked_eligible  BOOLEAN NOT NULL DEFAULT FALSE,
    first_seen_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    raw_data            JSONB,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_events_supercell_mode_map UNIQUE (supercell_id, mode, map_name)
);

CREATE INDEX idx_events_mode ON events(mode);

-- ---------------------------------------------------------------------------
-- players
-- Identity registry for all discovered player tags.
-- Not a snapshot - mutable name only; the snapshot tables hold immutable history.
-- ---------------------------------------------------------------------------
CREATE TABLE players (
    tag             VARCHAR(16) PRIMARY KEY,    -- without '#', uppercase
    name            VARCHAR(64),                -- last known display name (mutable)
    first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_crawled_at TIMESTAMPTZ,
    crawl_count     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_players_last_crawled_at ON players(last_crawled_at);

-- ---------------------------------------------------------------------------
-- player_snapshots
-- Immutable record of a player's profile state at each crawl time.
-- Never updated or deleted - the full history of trophy progression lives here.
-- ---------------------------------------------------------------------------
CREATE TABLE player_snapshots (
    id                  BIGSERIAL PRIMARY KEY,
    player_tag          VARCHAR(16) NOT NULL REFERENCES players(tag),
    snapshot_at         TIMESTAMPTZ NOT NULL,
    patch_id            INTEGER REFERENCES patches(id),
    trophies            INTEGER NOT NULL,
    highest_trophies    INTEGER NOT NULL,
    exp_level           INTEGER NOT NULL,
    three_v_three_wins  INTEGER NOT NULL,
    solo_victories      INTEGER NOT NULL,
    duo_victories       INTEGER NOT NULL,
    club_tag            VARCHAR(16),
    club_name           VARCHAR(64),
    trophy_bucket       SMALLINT NOT NULL,      -- derived at snapshot time (see domain/buckets.go)
    raw_data            JSONB NOT NULL,          -- full /players/{tag} API response

    CONSTRAINT uq_player_snapshots_player_time UNIQUE (player_tag, snapshot_at)
);

CREATE INDEX idx_player_snapshots_player_tag  ON player_snapshots(player_tag);
CREATE INDEX idx_player_snapshots_snapshot_at ON player_snapshots(snapshot_at DESC);
CREATE INDEX idx_player_snapshots_patch_id    ON player_snapshots(patch_id);
CREATE INDEX idx_player_snapshots_trophy_bucket ON player_snapshots(trophy_bucket);

-- ---------------------------------------------------------------------------
-- player_brawler_snapshots
-- Immutable record of a player's brawler state at the time of a snapshot.
-- One row per brawler per snapshot.
-- ---------------------------------------------------------------------------
CREATE TABLE player_brawler_snapshots (
    id               BIGSERIAL PRIMARY KEY,
    snapshot_id      BIGINT NOT NULL REFERENCES player_snapshots(id) ON DELETE CASCADE,
    brawler_id       INTEGER NOT NULL REFERENCES brawlers(id),
    power            SMALLINT NOT NULL,              -- 1-11
    rank             SMALLINT NOT NULL,              -- 1-35
    trophies         INTEGER NOT NULL,
    highest_trophies INTEGER NOT NULL,
    star_powers      INTEGER[] NOT NULL DEFAULT '{}',  -- IDs of owned star powers
    gadgets          INTEGER[] NOT NULL DEFAULT '{}',  -- IDs of owned gadgets
    gears            JSONB,                            -- gear structure uncertain; typed in future migration

    CONSTRAINT uq_player_brawler_snapshot UNIQUE (snapshot_id, brawler_id)
);

CREATE INDEX idx_player_brawler_snapshots_snapshot_id ON player_brawler_snapshots(snapshot_id);
CREATE INDEX idx_player_brawler_snapshots_brawler_id  ON player_brawler_snapshots(brawler_id);

-- ---------------------------------------------------------------------------
-- battles
-- Deduplicated match store. One row per unique real-world battle regardless
-- of how many players' logs it appeared in. See internal/ingestion/fingerprint.go.
-- ---------------------------------------------------------------------------
CREATE TABLE battles (
    id                         BIGSERIAL PRIMARY KEY,
    fingerprint                VARCHAR(64) NOT NULL UNIQUE,  -- SHA-256 of dedup key
    battle_time                TIMESTAMPTZ NOT NULL,
    event_id                   INTEGER REFERENCES events(id),
    event_mode                 VARCHAR(64) NOT NULL,          -- denormalized for query speed
    event_map                  VARCHAR(128) NOT NULL,         -- denormalized for query speed
    battle_type                VARCHAR(32) NOT NULL,          -- 'ranked','soloRanked','friendly','challenge'
    duration_seconds           INTEGER,
    star_player_tag            VARCHAR(16),
    patch_id                   INTEGER REFERENCES patches(id),
    trophy_change              INTEGER,                       -- from first-discovering player's perspective (display only)
    trophy_change_player_tag   VARCHAR(16),                   -- whose perspective provided trophy_change
    discovered_via_player_tag  VARCHAR(16) REFERENCES players(tag),
    raw_battle_data            JSONB NOT NULL,                -- full battle object from API
    first_seen_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_battles_battle_time          ON battles(battle_time DESC);
CREATE INDEX idx_battles_patch_time           ON battles(patch_id, battle_time DESC);
CREATE INDEX idx_battles_event_mode_map_patch ON battles(event_mode, event_map, patch_id);
CREATE INDEX idx_battles_battle_type          ON battles(battle_type);
CREATE INDEX idx_battles_event_id             ON battles(event_id);

-- ---------------------------------------------------------------------------
-- battle_teams
-- One row per team per battle. For 3v3: team_index 0 and 1.
-- For showdown modes: verify API structure before finalizing (may need different approach).
-- ---------------------------------------------------------------------------
CREATE TABLE battle_teams (
    id          BIGSERIAL PRIMARY KEY,
    battle_id   BIGINT NOT NULL REFERENCES battles(id) ON DELETE CASCADE,
    team_index  SMALLINT NOT NULL,  -- 0 or 1 for 3v3; 0-N for showdown
    result      VARCHAR(16),        -- 'victory','defeat','draw'

    CONSTRAINT uq_battle_teams_battle_team UNIQUE (battle_id, team_index)
);

CREATE INDEX idx_battle_teams_battle_id ON battle_teams(battle_id);

-- ---------------------------------------------------------------------------
-- battle_participants
-- One row per player per battle. The primary table for all analytics.
-- player_tag has NO FK - opponents may not be in the players table.
-- ---------------------------------------------------------------------------
CREATE TABLE battle_participants (
    id               BIGSERIAL PRIMARY KEY,
    battle_id        BIGINT NOT NULL REFERENCES battles(id) ON DELETE CASCADE,
    team_id          BIGINT NOT NULL REFERENCES battle_teams(id) ON DELETE CASCADE,
    player_tag       VARCHAR(16) NOT NULL,   -- no FK: opponent may not be tracked
    player_name      VARCHAR(64),
    brawler_id       INTEGER NOT NULL REFERENCES brawlers(id),
    brawler_power    SMALLINT NOT NULL,
    brawler_trophies INTEGER NOT NULL,
    is_star_player   BOOLEAN NOT NULL DEFAULT FALSE,
    trophy_bucket    SMALLINT NOT NULL,      -- derived from brawler_trophies at battle time

    CONSTRAINT uq_battle_participant UNIQUE (battle_id, player_tag)
);

CREATE INDEX idx_battle_participants_battle_id      ON battle_participants(battle_id);
CREATE INDEX idx_battle_participants_player_tag     ON battle_participants(player_tag);
CREATE INDEX idx_battle_participants_brawler_id     ON battle_participants(brawler_id);
CREATE INDEX idx_battle_participants_trophy_bucket  ON battle_participants(trophy_bucket);
CREATE INDEX idx_battle_participants_brawler_team   ON battle_participants(brawler_id, team_id);
CREATE INDEX idx_battle_participants_brawler_bucket ON battle_participants(brawler_id, trophy_bucket);
