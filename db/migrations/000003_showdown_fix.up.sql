-- Make team_id nullable so Solo Showdown participants can be stored without a team row.
-- The FK constraint still enforces referential integrity when team_id is provided (3v3 rows).
-- NULL team_id is used for Solo Showdown participants where no team structure exists.
ALTER TABLE battle_participants ALTER COLUMN team_id DROP NOT NULL;
