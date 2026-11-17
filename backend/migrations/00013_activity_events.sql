-- +goose Up
CREATE TABLE activity_releases (
 source_id bigint PRIMARY KEY, repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 name text NOT NULL, tag_name text NOT NULL, body text NOT NULL, html_url text NOT NULL,
 created_at timestamptz NOT NULL, updated_at timestamptz, published_at timestamptz NOT NULL, observed_at timestamptz NOT NULL,
 prerelease boolean NOT NULL, author_id bigint, author_login text, author_type text
);
CREATE INDEX activity_releases_repository_id ON activity_releases(repository_id, source_id);
CREATE TABLE activity_issue_events (
 source_id bigint PRIMARY KEY, repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 issue_id bigint NOT NULL, issue_number bigint NOT NULL, event text NOT NULL, url text NOT NULL,
 created_at timestamptz NOT NULL, observed_at timestamptz NOT NULL,
 actor_id bigint, actor_login text, actor_type text, assignee_id bigint, assignee_login text, assignee_type text, label text
);
CREATE INDEX activity_issue_events_repository_id ON activity_issue_events(repository_id, source_id);
-- +goose Down
DROP TABLE activity_issue_events;
DROP TABLE activity_releases;
