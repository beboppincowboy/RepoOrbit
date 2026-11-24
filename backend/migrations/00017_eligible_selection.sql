-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION is_eligible_selection(
    deselected_at timestamptz,
    follow_suspended_at timestamptz,
    is_demo boolean
) RETURNS boolean
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT deselected_at IS NULL
       AND follow_suspended_at IS NULL
       AND is_demo IS FALSE;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION is_eligible_selection(timestamptz, timestamptz, boolean);
