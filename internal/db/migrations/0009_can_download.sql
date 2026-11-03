-- Films and episodes are downloadable; scans before this wrote false, which
-- hides jellyfin-web's Download entry.
UPDATE items SET dto = jsonb_set(dto, '{CanDownload}', 'true') WHERE dto->>'MediaType' = 'Video';
