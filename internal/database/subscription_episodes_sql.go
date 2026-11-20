package database

const upsertSubscriptionEpisodesSQL = `
	WITH target AS MATERIALIZED (
		SELECT id
		FROM subscriptions
		WHERE id=$1 AND username=$2 AND completed_at IS NULL
		FOR UPDATE
	), requested AS MATERIALIZED (
		SELECT requested.season, requested.episode
		FROM unnest($3::integer[], $4::integer[]) AS requested(season, episode)
	), new_requested AS MATERIALIZED (
		SELECT requested.season, requested.episode
		FROM requested
		CROSS JOIN target
		WHERE NOT EXISTS (
			SELECT 1
			FROM subscription_episodes AS existing
			WHERE existing.subscription_id=target.id
				AND existing.season=requested.season
				AND existing.episode=requested.episode
		)
	), capacity AS (
		SELECT (
			(SELECT count(*) FROM subscription_episodes AS existing JOIN target ON target.id=existing.subscription_id)
			+ (SELECT count(*) FROM new_requested)
		) <= $5 AS allowed
	), inserted AS (
		INSERT INTO subscription_episodes (subscription_id, season, episode, state)
		SELECT target.id, requested.season, requested.episode, 'missing'
		FROM target
		CROSS JOIN requested
		WHERE COALESCE((SELECT allowed FROM capacity), FALSE)
		ON CONFLICT (subscription_id, season, episode) DO NOTHING
		RETURNING 1
	)
	SELECT EXISTS(SELECT 1 FROM target),
		COALESCE((SELECT allowed FROM capacity), FALSE),
		(SELECT count(*) FROM inserted)`

const listSubscriptionEpisodesSQL = `
	SELECT se.id, se.subscription_id, se.season, se.episode, se.state,
		se.download_intent_id, se.updated_at
	FROM subscription_episodes AS se
	JOIN subscriptions AS s ON s.id=se.subscription_id
	WHERE s.id=$1 AND s.username=$2
		AND se.state=ANY($3::text[])
	ORDER BY se.season, se.episode
	LIMIT $4`

const compareAndSwapSubscriptionEpisodeStateSQL = `
	WITH target AS MATERIALIZED (
		SELECT id, current_download_intent_id
		FROM subscriptions
		WHERE id=$1 AND username=$2 AND completed_at IS NULL
		FOR UPDATE
	), updated AS (
	UPDATE subscription_episodes AS se
	SET state=$5,
		download_intent_id=CASE WHEN $5='missing' THEN NULL ELSE se.download_intent_id END,
		updated_at=now()
	FROM target
	WHERE se.subscription_id=target.id
		AND (se.download_intent_id IS NULL OR target.current_download_intent_id=se.download_intent_id)
		AND se.season=$3 AND se.episode=$4 AND se.state=$6
	RETURNING se.id
	)
	SELECT id FROM updated`

const getActiveSubscriptionEpisodeStateSQL = `
	SELECT se.state
	FROM subscription_episodes AS se
	JOIN subscriptions AS s ON s.id=se.subscription_id
	WHERE se.subscription_id=$1 AND s.username=$2 AND s.completed_at IS NULL
		AND (se.download_intent_id IS NULL OR s.current_download_intent_id=se.download_intent_id)
		AND se.season=$3 AND se.episode=$4`

const bindSubscriptionEpisodesSQL = `
	WITH target AS MATERIALIZED (
		SELECT id
		FROM subscriptions
		WHERE id=$1 AND username=$2 AND completed_at IS NULL
			AND current_download_intent_id=$3
		FOR UPDATE
	), intent AS MATERIALIZED (
		SELECT di.id
		FROM download_intents AS di
		JOIN target ON target.id=di.subscription_id
		WHERE di.id=$3 AND di.username=$2
		FOR UPDATE OF di
	), requested AS MATERIALIZED (
		SELECT requested.season, requested.episode
		FROM unnest($4::integer[], $5::integer[]) AS requested(season, episode)
	), matched AS MATERIALIZED (
		SELECT se.id, se.state, se.download_intent_id
		FROM subscription_episodes AS se
		JOIN target ON target.id=se.subscription_id
		JOIN requested ON requested.season=se.season AND requested.episode=se.episode
	), eligible AS MATERIALIZED (
		SELECT matched.id
		FROM matched
		WHERE matched.download_intent_id=$3
			OR (
				matched.download_intent_id IS NULL
				AND matched.state IN ('missing', 'acquired')
			)
	), counts AS (
		SELECT
			(SELECT count(*) FROM requested) AS requested_count,
			(SELECT count(*) FROM matched) AS matched_count,
			(SELECT count(*) FROM eligible) AS eligible_count
	), updated AS (
		UPDATE subscription_episodes AS se
		SET state=CASE WHEN se.state='missing' THEN 'acquired' ELSE se.state END,
			download_intent_id=$3,
			updated_at=CASE
				WHEN se.download_intent_id IS NULL OR se.state='missing' THEN now()
				ELSE se.updated_at
			END
		FROM eligible, counts
		WHERE se.id=eligible.id
			AND EXISTS(SELECT 1 FROM intent)
			AND counts.requested_count=counts.matched_count
			AND counts.requested_count=counts.eligible_count
		RETURNING 1
	)
	SELECT EXISTS(SELECT 1 FROM target),
		EXISTS(SELECT 1 FROM intent),
		counts.requested_count,
		counts.matched_count,
		counts.eligible_count,
		(SELECT count(*) FROM updated)
	FROM counts`

const verifySubscriptionEpisodesBoundToIntentSQL = `
	WITH target AS MATERIALIZED (
		SELECT s.id
		FROM subscriptions AS s
		JOIN download_intents AS di
			ON di.subscription_id=s.id AND di.id=$3 AND di.username=$2
		WHERE s.id=$1 AND s.username=$2 AND s.completed_at IS NULL
			AND s.current_download_intent_id=di.id
			AND s.internal_state IN (
				'acquired', 'downloading', 'pending_organization', 'organizing', 'pending_index', 'indexed'
			)
		FOR UPDATE OF s, di
	), requested AS MATERIALIZED (
		SELECT requested.season, requested.episode
		FROM unnest($4::integer[], $5::integer[]) AS requested(season, episode)
	), matched AS (
		SELECT count(*) AS count
		FROM subscription_episodes AS se
		JOIN target ON target.id=se.subscription_id
		JOIN requested ON requested.season=se.season AND requested.episode=se.episode
		WHERE se.download_intent_id=$3 AND se.state IN (
			'acquired', 'downloading', 'pending_organization', 'organizing', 'pending_index', 'indexed'
		)
	), bound AS (
		SELECT count(*) AS count
		FROM subscription_episodes AS se
		JOIN target ON target.id=se.subscription_id
		WHERE se.download_intent_id=$3
	)
	SELECT EXISTS(SELECT 1 FROM target),
		(SELECT count(*) FROM requested),
		(SELECT count FROM matched),
		(SELECT count FROM bound)`

const transitionSubscriptionEpisodesByIntentSQL = `
	WITH locked_intent AS MATERIALIZED (
		SELECT id, subscription_id
		FROM download_intents
		WHERE id=$1 AND username=$2
		FOR UPDATE
	), target AS MATERIALIZED (
		SELECT di.id, di.subscription_id
		FROM locked_intent AS di
		JOIN subscriptions AS s ON s.id=di.subscription_id
		WHERE s.username=$2 AND s.completed_at IS NULL
			AND s.current_download_intent_id=di.id
		FOR UPDATE OF s
	), bound AS MATERIALIZED (
		SELECT se.id, se.state
		FROM subscription_episodes AS se
		JOIN target ON target.subscription_id=se.subscription_id
		WHERE se.download_intent_id=target.id
		FOR UPDATE OF se
	), state_set AS (
		SELECT count(*) FILTER (WHERE bound.state=ANY($3::text[])) AS matched_count,
			count(*) FILTER (WHERE bound.state<>ALL($5::text[])) AS incompatible_count
		FROM bound
	), updated AS (
		UPDATE subscription_episodes AS se
		SET state=$4,
			download_intent_id=CASE WHEN $4='missing' THEN NULL ELSE se.download_intent_id END,
			updated_at=now()
		FROM target, state_set
		WHERE se.subscription_id=target.subscription_id
			AND se.download_intent_id=target.id
			AND se.state=ANY($3::text[])
			AND (state_set.matched_count=0 OR state_set.incompatible_count=0)
		RETURNING se.id
	)
	SELECT EXISTS(SELECT 1 FROM target),
		(SELECT matched_count=0 OR incompatible_count=0 FROM state_set),
		(SELECT count(*) FROM updated)`

const summarizeSubscriptionEpisodesSQL = `
	SELECT count(se.id),
		count(se.id) FILTER (WHERE se.state='missing'),
		count(se.id) FILTER (WHERE se.state='acquired'),
		count(se.id) FILTER (WHERE se.state='downloading'),
		count(se.id) FILTER (WHERE se.state='pending_organization'),
		count(se.id) FILTER (WHERE se.state='organizing'),
		count(se.id) FILTER (WHERE se.state='pending_index'),
		count(se.id) FILTER (WHERE se.state='indexed')
	FROM subscriptions AS s
	LEFT JOIN subscription_episodes AS se ON se.subscription_id=s.id
	WHERE s.id=$1 AND s.username=$2
	GROUP BY s.id`
