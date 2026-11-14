-- What a worker has and a job costs, in cores (playback.Spec.Cost), where
-- slots were counted. A row written by the release before has no cores (0), and
-- is then counted at its slots; a worker of this release writes both, slots as
-- the cores rounded up, so that a replica of the release before, which counts a
-- worker by slots > 0, still sees it while the workers roll first.
ALTER TABLE transcode_nodes ADD COLUMN cores numeric NOT NULL DEFAULT 0;
-- A job's cost is written when it is queued. A job queued by a replica of the
-- release before has none, and is costed from its spec wherever it is read.
ALTER TABLE transcode_jobs ADD COLUMN cost numeric;
