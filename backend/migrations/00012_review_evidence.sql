-- +goose Up
ALTER TABLE activity_issues ALTER COLUMN author_id DROP NOT NULL, ALTER COLUMN author_login DROP NOT NULL, ALTER COLUMN author_type DROP NOT NULL;
UPDATE activity_issues SET author_id=NULL, author_login=NULL, author_type=NULL WHERE author_id=0 AND author_login='' AND author_type='';
CREATE TABLE activity_pull_requests (
 source_id bigint PRIMARY KEY,
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 number bigint NOT NULL,
 title text NOT NULL,
 state text NOT NULL,
 html_url text NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 closed_at timestamptz,
 merged_at timestamptz,
 author_id bigint,
 author_login text,
 author_type text
);
CREATE INDEX activity_pull_requests_repository_id ON activity_pull_requests(repository_id, source_id);
CREATE TABLE activity_reviews (
 source_id bigint PRIMARY KEY,
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 pull_request_id bigint NOT NULL REFERENCES activity_pull_requests(source_id) ON DELETE CASCADE,
 state text NOT NULL,
 body text NOT NULL,
 html_url text NOT NULL,
 submitted_at timestamptz NOT NULL,
 author_id bigint,
 author_login text,
 author_type text
);
CREATE INDEX activity_reviews_repository_id ON activity_reviews(repository_id, source_id);
CREATE TABLE activity_comments (
 source_id bigint PRIMARY KEY,
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 issue_number bigint NOT NULL,
 body text NOT NULL,
 html_url text NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 author_id bigint,
 author_login text,
 author_type text
);
CREATE INDEX activity_comments_repository_id ON activity_comments(repository_id, source_id);
CREATE TABLE activity_children (
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 resource text NOT NULL CHECK (resource IN ('reviews', 'comments')),
 parent_id bigint NOT NULL,
 parent_number bigint NOT NULL,
 cursor text NOT NULL DEFAULT '',
 generation bigint NOT NULL DEFAULT 0,
 complete boolean NOT NULL DEFAULT false,
 scan_started_at timestamptz,
 watermark timestamptz,
 previous_scan bigint[],
 current_scan bigint[] NOT NULL DEFAULT '{}',
 last_success_at timestamptz,
 last_error_code text,
 PRIMARY KEY (repository_id, resource, parent_id)
);
-- +goose Down
DROP TABLE activity_children;
DROP TABLE activity_comments;
DROP TABLE activity_reviews;
DROP TABLE activity_pull_requests;

UPDATE activity_issues SET author_id=COALESCE(author_id,0), author_login=COALESCE(author_login,''), author_type=COALESCE(author_type,'') WHERE author_id IS NULL OR author_login IS NULL OR author_type IS NULL;
ALTER TABLE activity_issues ALTER COLUMN author_id SET NOT NULL, ALTER COLUMN author_login SET NOT NULL, ALTER COLUMN author_type SET NOT NULL;
