-- One worker at a time looks for the dead (JS-030): the reaper holds this one row
-- until its lease runs out, renewing it every look while it is fit to look, and
-- another worker takes the role when it has lapsed. A worker killed, stopped or cut
-- off from the database stops renewing, so the role never rests with a worker that
-- cannot do it.
CREATE TABLE transcode_reaper (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    holder    text NOT NULL,
    until     timestamptz NOT NULL
);
-- The session a worker's row belongs to: one for every time the row is put back
-- after another replica forgot it, so that a claim sent in an earlier session
-- cannot commit in a later one. Empty for a row of a worker from before it.
ALTER TABLE transcode_nodes ADD COLUMN session text NOT NULL DEFAULT '';
