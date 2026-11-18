-- github-api-proxy: request log and caller identities.
-- Applied automatically at startup (tracked in github_proxy_migrations).

CREATE TABLE IF NOT EXISTS github_proxy_requests (
    id                   BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    requested_at         DATETIME(3)      NOT NULL,
    duration_ms          INT UNSIGNED     NOT NULL,
    method               VARCHAR(10)      NOT NULL,
    path                 VARCHAR(1024)    NOT NULL,
    route                VARCHAR(255)     NOT NULL,
    query                VARCHAR(1024)    NULL,
    graphql_operation    VARCHAR(255)     NULL,
    status               SMALLINT UNSIGNED NOT NULL,
    request_bytes        BIGINT UNSIGNED  NOT NULL DEFAULT 0,
    response_bytes       BIGINT UNSIGNED  NOT NULL DEFAULT 0,
    client_ip            VARCHAR(45)      NOT NULL,
    user_agent           VARCHAR(255)     NULL,
    token_hash           CHAR(64)         NULL,
    token_kind           VARCHAR(20)      NOT NULL,
    rate_limit_resource  VARCHAR(40)      NULL,
    rate_limit_limit     INT UNSIGNED     NULL,
    rate_limit_remaining INT UNSIGNED     NULL,
    rate_limit_used      INT UNSIGNED     NULL,
    rate_limit_reset     DATETIME         NULL,
    github_request_id    VARCHAR(64)      NULL,
    error                VARCHAR(512)     NULL,
    PRIMARY KEY (id),
    KEY idx_requested_at (requested_at),
    KEY idx_token_time (token_hash, requested_at),
    KEY idx_status_time (status, requested_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS github_proxy_tokens (
    token_hash    CHAR(64)     NOT NULL,
    token_kind    VARCHAR(20)  NOT NULL,
    caller        VARCHAR(255) NULL,
    login         VARCHAR(255) NULL,
    account_type  VARCHAR(20)  NULL,
    note          VARCHAR(255) NULL,
    resolve_error VARCHAR(255) NULL,
    first_seen_at DATETIME(3)  NOT NULL,
    last_seen_at  DATETIME(3)  NOT NULL,
    resolved_at   DATETIME(3)  NULL,
    PRIMARY KEY (token_hash),
    KEY idx_caller (caller),
    KEY idx_last_seen (last_seen_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
