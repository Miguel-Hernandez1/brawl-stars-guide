-- Seed the initial patch record.
-- Update version and released_at when a new patch drops.
-- Only one row should have is_current = TRUE (enforced by partial unique index).
--
-- Run manually after applying migrations:
--   make psql
--   \i db/seeds/patches.sql
INSERT INTO patches (version, released_at, is_current, notes)
VALUES ('initial', NOW(), TRUE, 'Placeholder patch record for data collected before patch tracking is configured.')
ON CONFLICT (version) DO NOTHING;
