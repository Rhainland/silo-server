-- +goose Up
ALTER TABLE public.users
    ADD COLUMN audio_transcode_allowed boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE public.users
    DROP COLUMN audio_transcode_allowed;
