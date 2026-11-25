-- +goose Up
-- Each stored issue's Board state (R15), the one derivation the Board and Assigned to me read: 'done' when closed
-- whatever its labels, else the furthest-along status label ('in_review', 'in_progress', 'todo'), else 'backlog'.
-- multiple_status flags an open issue carrying more than one status label. It ignores the repository's status labels
-- flag: each query applies repositories.status_labels itself.
CREATE VIEW issue_board_states AS
SELECT i.source_id AS issue_id,
 (CASE WHEN i.state = 'closed' THEN 'done' WHEN s.top = 3 THEN 'in_review' WHEN s.top = 2 THEN 'in_progress' WHEN s.top = 1 THEN 'todo' ELSE 'backlog' END)::text AS status,
 (i.state <> 'closed' AND s.n > 1)::boolean AS multiple_status
FROM activity_issues i
CROSS JOIN LATERAL (SELECT max(status_label_rank(l.name)) AS top, count(DISTINCT status_label_rank(l.name)) AS n
 FROM activity_issue_labels l WHERE l.issue_id = i.source_id) s;

-- +goose Down
DROP VIEW issue_board_states;
