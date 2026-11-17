-- +goose Up
-- A monotonic global permissions version: every authorization bump also increments scope 'global', subject 0, so a
-- private commit whose snapshot predates any bump (including for an installation never saved locally) is withheld.
ALTER TABLE access_fences DROP CONSTRAINT access_fences_scope_check;
ALTER TABLE access_fences ADD CONSTRAINT access_fences_scope_check CHECK (scope IN ('user', 'installation', 'repository', 'global'));
-- +goose Down
DELETE FROM access_fences WHERE scope = 'global';
ALTER TABLE access_fences DROP CONSTRAINT access_fences_scope_check;
ALTER TABLE access_fences ADD CONSTRAINT access_fences_scope_check CHECK (scope IN ('user', 'installation', 'repository'));
