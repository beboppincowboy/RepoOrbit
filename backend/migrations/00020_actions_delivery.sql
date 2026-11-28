-- +goose Up
ALTER TABLE activity_pull_requests ADD COLUMN head_sha text NOT NULL DEFAULT '';
CREATE INDEX activity_pull_requests_repository_number ON activity_pull_requests (repository_id, number);
CREATE INDEX activity_pull_requests_repository_head_sha ON activity_pull_requests (repository_id, head_sha);

CREATE TABLE workflows (
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 source_id bigint NOT NULL,
 name text NOT NULL,
 path text NOT NULL,
 state text NOT NULL,
 html_url text NOT NULL,
 updated_at timestamptz NOT NULL,
 PRIMARY KEY (repository_id, source_id)
);
CREATE TABLE workflow_runs (
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 source_id bigint NOT NULL,
 workflow_id bigint NOT NULL,
 workflow_name text NOT NULL,
 run_number bigint NOT NULL,
 event text NOT NULL,
 status text NOT NULL,
 conclusion text NOT NULL,
 head_branch text NOT NULL,
 head_sha text NOT NULL,
 html_url text NOT NULL,
 created_at timestamptz NOT NULL,
 run_started_at timestamptz,
 updated_at timestamptz NOT NULL,
 pull_request_numbers bigint[] NOT NULL DEFAULT '{}',
 PRIMARY KEY (repository_id, source_id)
);
CREATE INDEX workflow_runs_repository_created ON workflow_runs (repository_id, created_at DESC);
CREATE INDEX workflow_runs_repository_workflow_created ON workflow_runs (repository_id, workflow_id, created_at DESC);
CREATE INDEX workflow_runs_repository_head_sha ON workflow_runs (repository_id, head_sha);
CREATE TABLE workflow_jobs (
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 source_id bigint NOT NULL,
 run_id bigint NOT NULL,
 name text NOT NULL,
 status text NOT NULL,
 conclusion text NOT NULL,
 html_url text NOT NULL,
 created_at timestamptz NOT NULL,
 started_at timestamptz,
 completed_at timestamptz,
 PRIMARY KEY (repository_id, source_id)
);
CREATE INDEX workflow_jobs_repository_run ON workflow_jobs (repository_id, run_id);
CREATE TABLE deployments (
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 source_id bigint NOT NULL,
 environment text NOT NULL,
 sha text NOT NULL,
 ref text NOT NULL,
 state text NOT NULL,
 description text NOT NULL,
 html_url text NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 PRIMARY KEY (repository_id, source_id)
);
CREATE INDEX deployments_repository_created ON deployments (repository_id, created_at DESC);
CREATE INDEX deployments_repository_sha ON deployments (repository_id, sha);
CREATE TABLE issue_pull_request_links (
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 issue_number bigint NOT NULL,
 pull_request_number bigint NOT NULL,
 source text NOT NULL,
 created_at timestamptz NOT NULL,
 PRIMARY KEY (repository_id, issue_number, pull_request_number)
);

-- +goose Down
DROP TABLE issue_pull_request_links;
DROP TABLE deployments;
DROP TABLE workflow_jobs;
DROP TABLE workflow_runs;
DROP TABLE workflows;
DROP INDEX activity_pull_requests_repository_head_sha;
DROP INDEX activity_pull_requests_repository_number;
ALTER TABLE activity_pull_requests DROP COLUMN head_sha;
