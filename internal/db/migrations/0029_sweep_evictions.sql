-- The specs whose removal from the transcode store has started (JS-078). A
-- sweep that dies partway leaves a spec in part; the next sweep finishes every
-- spec listed here that nobody wants again, even when the store is under its
-- budget by then.
CREATE TABLE sweep_evictions (
    spec_hash text PRIMARY KEY
);
