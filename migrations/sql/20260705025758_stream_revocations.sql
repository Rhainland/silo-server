-- +goose Up
-- +goose StatementBegin
CREATE TABLE public.stream_revocations (
    kind text NOT NULL,
    id text NOT NULL,
    reason text NOT NULL DEFAULT '',
    revoked_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CONSTRAINT stream_revocations_pkey PRIMARY KEY (kind, id),
    CONSTRAINT stream_revocations_kind_check CHECK (kind IN ('sess', 'user'))
);

CREATE INDEX stream_revocations_expires_at_idx
    ON public.stream_revocations (expires_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS public.stream_revocations;
-- +goose StatementEnd
