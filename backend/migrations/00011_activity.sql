-- +goose Up
CREATE TABLE activity_resources (
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 resource text NOT NULL,
 boundary timestamptz NOT NULL,
 phase text NOT NULL DEFAULT 'backfill' CHECK (phase IN ('backfill', 'catchup', 'current')),
 page integer NOT NULL DEFAULT 1 CHECK (page > 0),
 cursor text NOT NULL DEFAULT '',
 generation bigint NOT NULL DEFAULT 0,
 watermark timestamptz,
 scan_started_at timestamptz,
 previous_scan bigint[],
 current_scan bigint[] NOT NULL DEFAULT '{}',
 last_success_at timestamptz,
 last_error_code text,
 PRIMARY KEY (repository_id, resource)
);
CREATE TABLE activity_issues (
 source_id bigint PRIMARY KEY,
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 number bigint NOT NULL,
 title text NOT NULL,
 state text NOT NULL,
 html_url text NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 closed_at timestamptz,
 author_id bigint NOT NULL,
 author_login text NOT NULL,
 author_type text NOT NULL
);
CREATE INDEX activity_issues_repository_id ON activity_issues (repository_id, source_id);
CREATE TABLE activity_issue_labels (
 issue_id bigint NOT NULL REFERENCES activity_issues(source_id) ON DELETE CASCADE,
 source_id bigint NOT NULL,
 name text NOT NULL,
 PRIMARY KEY (issue_id, source_id)
);
CREATE TABLE activity_issue_assignees (
 issue_id bigint NOT NULL REFERENCES activity_issues(source_id) ON DELETE CASCADE,
 source_id bigint NOT NULL,
 login text NOT NULL,
 type text NOT NULL,
 PRIMARY KEY (issue_id, source_id)
);
-- Security work survives job deferral, worker restarts, and an absence of selectors.
CREATE TABLE activity_cleanup (
 repository_id bigint PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
 version bigint NOT NULL DEFAULT 1
);
-- +goose Down
DROP TABLE activity_cleanup;
DROP TABLE activity_issue_assignees;
DROP TABLE activity_issue_labels;
DROP TABLE activity_issues;
DROP TABLE activity_resources;
