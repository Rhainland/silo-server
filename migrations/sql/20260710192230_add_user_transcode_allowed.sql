-- +goose Up
ALTER TABLE public.users
    ADD COLUMN transcode_allowed boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE public.users
    DROP COLUMN transcode_allowed;
