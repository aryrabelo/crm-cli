-- up

-- Data plane (fork): a run is executed by one process at a time. owner
-- identifies the executor holding the lease (host:pid:nonce) and
-- heartbeat_at (unix milliseconds) is renewed while it works; another
-- executor may take the run over only once the heartbeat is stale.
ALTER TABLE runs ADD COLUMN owner TEXT;
ALTER TABLE runs ADD COLUMN heartbeat_at INTEGER;

-- down

ALTER TABLE runs DROP COLUMN heartbeat_at;
ALTER TABLE runs DROP COLUMN owner;
