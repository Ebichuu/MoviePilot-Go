package database

// Movie completion also consumes an already accepted terminal best-version
// quality, e.g. after a P subscription is identified and enters N/R.
// Keep TV's ordinary-library predicate separate and never overwrite progress.
const completeSatisfiedMovieSubscriptionSQL = `
	UPDATE subscriptions
	SET internal_state=$3, v2_state=$4, completed_at=now(),
		config=config || $7::jsonb, updated_at=now()
	WHERE id=$1 AND username=$2 AND completed_at IS NULL
		AND current_download_intent_id IS NULL
		AND jsonb_typeof(config)='object'
		AND internal_state=$5
		AND updated_at=$6
		AND LOWER(BTRIM(media_type)) IN ($8, $9)
		AND (
			NOT CASE jsonb_typeof(config->'best_version')
				WHEN 'boolean' THEN (config->>'best_version')::boolean
				WHEN 'number' THEN
					(config->>'best_version')::numeric = TRUNC((config->>'best_version')::numeric)
					AND (config->>'best_version')::numeric <> 0
				ELSE false
			END
			OR CASE jsonb_typeof(config->'current_priority')
				WHEN 'number' THEN (config->>'current_priority')::numeric = 100
				ELSE false
			END
		)
	RETURNING id`
