-- +goose Up
ALTER TABLE repositories ADD COLUMN archived boolean NOT NULL DEFAULT false;
-- A signed deletion is positive evidence: the id can never be imported again, even from a stale page.
CREATE TABLE activity_tombstones (
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 resource text NOT NULL CHECK (resource IN ('issues', 'comments', 'releases')),
 source_id bigint NOT NULL,
 deleted_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (resource, source_id)
);
-- +goose StatementBegin
CREATE FUNCTION activity_skip_tombstoned() RETURNS trigger AS $$
BEGIN
 IF EXISTS (SELECT 1 FROM activity_tombstones WHERE resource = TG_ARGV[0] AND source_id = NEW.source_id) THEN
  RETURN NULL;
 END IF;
 RETURN NEW;
END
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER activity_issues_tombstone BEFORE INSERT ON activity_issues FOR EACH ROW EXECUTE FUNCTION activity_skip_tombstoned('issues');
CREATE TRIGGER activity_comments_tombstone BEFORE INSERT ON activity_comments FOR EACH ROW EXECUTE FUNCTION activity_skip_tombstoned('comments');
CREATE TRIGGER activity_releases_tombstone BEFORE INSERT ON activity_releases FOR EACH ROW EXECUTE FUNCTION activity_skip_tombstoned('releases');
-- +goose Down
DROP TRIGGER activity_releases_tombstone ON activity_releases;
DROP TRIGGER activity_comments_tombstone ON activity_comments;
DROP TRIGGER activity_issues_tombstone ON activity_issues;
DROP FUNCTION activity_skip_tombstoned();
DROP TABLE activity_tombstones;
ALTER TABLE repositories DROP COLUMN archived;
