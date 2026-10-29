-- A tool call carries how its approval was answered in its meta
-- ($.approval: decision, auto, answers), which the projection of
-- approval.resolved now writes. This fills it in for the calls answered
-- before.
UPDATE items SET meta = json_set(items.meta, '$.approval', json_object(
	'decision', a.decision,
	'auto', json(CASE WHEN (SELECT json_extract(e.payload, '$.auto') FROM events e WHERE e.thread_id = a.thread_id AND e.type = 'approval.resolved' AND json_extract(e.payload, '$.id') = a.id) THEN 'true' ELSE 'false' END),
	'answers', CASE WHEN a.answers = '' THEN NULL ELSE json(a.answers) END))
FROM approvals a
WHERE a.item_id = items.id AND a.decision != '';
