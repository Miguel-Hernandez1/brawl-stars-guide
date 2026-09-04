-- Duels (tagTeam mode) battles have no individual brawler attribution in the API.
-- The API returns brawler.id=0 as a sentinel for this mode.
-- Make brawler columns nullable so we can store the participant identity (tag + name)
-- while honestly representing that brawler data is unavailable.
-- Precedent: team_id was made nullable in 000003 for the same reason (no team structure in Showdown).
ALTER TABLE battle_participants ALTER COLUMN brawler_id       DROP NOT NULL;
ALTER TABLE battle_participants ALTER COLUMN brawler_power    DROP NOT NULL;
ALTER TABLE battle_participants ALTER COLUMN brawler_trophies DROP NOT NULL;
ALTER TABLE battle_participants ALTER COLUMN trophy_bucket    DROP NOT NULL;
