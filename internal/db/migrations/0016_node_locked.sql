-- Whether the node holds a liveness lock (transcode.Liveness). Only a node that
-- does can be told dead by its lock being gone; an older worker, which holds
-- none, is told dead by its lease alone.
ALTER TABLE transcode_nodes ADD COLUMN locked boolean NOT NULL DEFAULT false;
