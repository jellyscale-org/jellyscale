-- The expiry predicates use CASE WHEN approved_at IS NOT NULL THEN approved_at ELSE created_at END, which no index on
-- the two columns alone can serve. At most 100 codes are live and expired ones are removed by the next Initiate, so a
-- scan of the small table costs less than maintaining an index nothing uses.
DROP INDEX IF EXISTS quickconnect_expiry_key;
