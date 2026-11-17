-- +goose Up
CREATE TABLE repositories (
 id bigint PRIMARY KEY,
 name text NOT NULL,
 description text NOT NULL
);
CREATE TABLE issues (
 id bigint PRIMARY KEY,
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 title text NOT NULL,
 state text NOT NULL
);
CREATE INDEX issues_repository_id_id ON issues (repository_id, id);
-- +goose Down
DROP TABLE issues;
DROP TABLE repositories;
