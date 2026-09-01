-- Seed the initial patch record.
-- Update version and released_at when a new patch drops.
-- Only one row should have is_current = TRUE (enforced by partial unique index).
--
-- released_at is set to 2017-01-01 so that the time-aware PatchIDForTime query
-- returns this row for all historical battles (Brawl Stars launched in 2017).
-- Replace with real patch records as patch tracking is configured.
--
-- Run manually after applying migrations:
--   make psql
--   \i db/seeds/patches.sql
INSERT INTO patches (version, released_at, is_current, notes)
VALUES ('initial', '2017-01-01 00:00:00+00', TRUE, 'Placeholder patch record. Replace with real patch records when patch tracking is configured.')
ON CONFLICT (version) DO UPDATE SET released_at = EXCLUDED.released_at, notes = EXCLUDED.notes;
