-- Quick Connect (JS-037): one row per code, shared by every api replica. The device that asked for it is stored with the
-- code, so a redemption with any header signs that device in. Both windows are from a stored timestamptz, which a
-- replica never keeps in memory, so expiry holds across replicas and restarts.
CREATE TABLE quickconnect_codes (
    secret text PRIMARY KEY,
    code text NOT NULL,
    device_id text NOT NULL,
    device_name text NOT NULL,
    app_name text NOT NULL,
    app_version text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    approved_at timestamptz,
    approved_by uuid,
    token text
);
-- A code is never live for two devices; Initiate removes expired rows first, so a code that is still live is kept out.
CREATE UNIQUE INDEX quickconnect_code_key ON quickconnect_codes (code);
-- The expiry predicates use CASE WHEN approved_at IS NOT NULL THEN approved_at ELSE created_at END, which no index on
-- the two columns alone can serve. At most 100 codes are live and expired ones are removed by the next Initiate, so a
-- scan of the small table costs less than maintaining an index nothing uses.
