CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Only the hash of a refresh token is stored. Tokens issued by rotating one
-- login share a family, so a reused token can revoke the whole family.
CREATE TABLE refresh_tokens (
    token_hash BYTEA PRIMARY KEY,
    family_id  UUID NOT NULL,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
CREATE INDEX refresh_tokens_family ON refresh_tokens (family_id);

CREATE TABLE rooms (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('public', 'dm')),
    dm_key        TEXT UNIQUE, -- "lowUserID:highUserID", so two users share one DM
    created_by    BIGINT REFERENCES users (id),
    last_sequence BIGINT NOT NULL DEFAULT 0, -- sequence of the newest message
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX rooms_public_name ON rooms (name) WHERE kind = 'public' AND deleted_at IS NULL;

CREATE TABLE memberships (
    room_id   BIGINT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    user_id   BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, user_id)
);
CREATE INDEX memberships_user ON memberships (user_id);

CREATE TABLE messages (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    room_id           BIGINT NOT NULL REFERENCES rooms (id),
    sequence          BIGINT NOT NULL,
    sender_id         BIGINT NOT NULL REFERENCES users (id),
    client_message_id UUID NOT NULL,
    content           TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (room_id, sequence),
    UNIQUE (sender_id, client_message_id) -- makes a retried send idempotent
);

-- Events are written in the same transaction as the change they describe.
-- The publisher process forwards them to Redis afterwards.
CREATE TABLE outbox (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event        TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);
CREATE INDEX outbox_pending ON outbox (id) WHERE published_at IS NULL;

-- Wake the publisher as soon as a transaction with new events commits.
CREATE FUNCTION notify_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NOTIFY outbox;
    RETURN NULL;
END $$;
CREATE TRIGGER outbox_notify AFTER INSERT ON outbox
    FOR EACH STATEMENT EXECUTE FUNCTION notify_outbox();

INSERT INTO rooms (name, kind) VALUES ('lobby', 'public');
