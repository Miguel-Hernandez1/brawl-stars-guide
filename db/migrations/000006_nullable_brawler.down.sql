-- Reverting this migration will fail if any rows have NULL brawler_id (Duels participants).
-- Delete or update those rows first if a rollback is needed.
ALTER TABLE battle_participants ALTER COLUMN trophy_bucket    SET NOT NULL;
ALTER TABLE battle_participants ALTER COLUMN brawler_trophies SET NOT NULL;
ALTER TABLE battle_participants ALTER COLUMN brawler_power    SET NOT NULL;
ALTER TABLE battle_participants ALTER COLUMN brawler_id       SET NOT NULL;
