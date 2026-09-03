-- Restore NOT NULL on team_id.
-- Showdown participant rows (team_id = NULL) must be removed first or this will fail.
DELETE FROM battle_participants WHERE team_id IS NULL;
ALTER TABLE battle_participants ALTER COLUMN team_id SET NOT NULL;
