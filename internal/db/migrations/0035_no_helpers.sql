-- Whether a worker's jobs ask for no helpers (transcodeHelpers false). A replica reads it to know whether a play can be
-- shared between workers (JS-161, D-046). A row written by the release before has the default, which counts as asking.
ALTER TABLE transcode_nodes ADD COLUMN no_helpers boolean NOT NULL DEFAULT false;
