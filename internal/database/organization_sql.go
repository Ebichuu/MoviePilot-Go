package database

const organizationBatchColumns = `
	b.id, b.intent_id, b.state, b.source_root, b.target_root, b.plan,
	COALESCE(b.last_error, ''), b.attempt_count, b.next_attempt_at,
	b.lease_token, b.lease_expires_at, b.completed_at, b.created_at, b.updated_at`

const organizationFileColumns = `
	f.id, f.batch_id, f.ordinal, f.source_path, f.target_path, f.mode,
	f.expected_size, f.expected_sha256, f.state, f.attempt_count,
	f.last_error, f.completed_at, f.updated_at`

const createOrganizationBatchSQL = `
	INSERT INTO organization_batches (
		id, intent_id, state, source_root, target_root, plan, last_error, next_attempt_at
	)
	SELECT $1, di.id, 'planned', $4, $5, $6::jsonb, '', now()
	FROM download_intents AS di
	LEFT JOIN subscriptions AS s ON s.id=di.subscription_id AND s.username=di.username
	WHERE di.id=$2 AND di.username=$3 AND di.state='completed'
		AND (di.subscription_id IS NULL OR s.id IS NOT NULL)
	ON CONFLICT (intent_id) WHERE intent_id IS NOT NULL DO NOTHING
	RETURNING id, intent_id, state, source_root, target_root, plan,
		COALESCE(last_error, ''), attempt_count, next_attempt_at,
		lease_token, lease_expires_at, completed_at, created_at, updated_at`

const getOrganizationBatchByIDSQL = `
	SELECT ` + organizationBatchColumns + `
	FROM organization_batches AS b
	JOIN download_intents AS di ON di.id=b.intent_id
	WHERE b.id=$1 AND di.username=$2`

const getOrganizationBatchByIntentSQL = `
	SELECT ` + organizationBatchColumns + `
	FROM organization_batches AS b
	JOIN download_intents AS di ON di.id=b.intent_id
	WHERE b.intent_id=$1 AND di.username=$2`

const listDueOrganizationBatchIDsSQL = `
	SELECT id
	FROM organization_batches
	WHERE (attempt_count < $3
			OR last_error IN ('media server unavailable, index confirmation deferred',
				'media server path mapping missing')
		)
		AND (
			(state IN ('planned', 'failed') AND next_attempt_at <= $1)
			OR (state='organizing' AND lease_expires_at <= $1)
		)
	ORDER BY next_attempt_at, updated_at, id
	LIMIT $2`

const claimOrganizationBatchesSQL = `
	WITH candidates AS MATERIALIZED (
		SELECT b.id
		FROM organization_batches AS b
		JOIN download_intents AS di ON di.id=b.intent_id AND di.state='completed'
		LEFT JOIN subscriptions AS s ON s.id=di.subscription_id AND s.username=di.username
		WHERE (
				b.state IN ('planned', 'failed')
				OR (b.state='organizing' AND b.lease_expires_at <= $1)
			)
			AND (b.state='organizing' OR b.next_attempt_at <= $1)
			AND b.attempt_count < $5
			AND (di.subscription_id IS NULL OR s.id IS NOT NULL)
		ORDER BY b.next_attempt_at, b.updated_at, b.id
		FOR UPDATE OF b SKIP LOCKED
		LIMIT $2
	), locked_parents AS MATERIALIZED (
		SELECT s.id AS subscription_id, di.id AS intent_id
		FROM subscriptions AS s
		JOIN download_intents AS di
			ON di.subscription_id=s.id AND di.username=s.username
		JOIN organization_batches AS b ON b.intent_id=di.id
		JOIN candidates ON candidates.id=b.id
		WHERE s.completed_at IS NULL AND s.current_download_intent_id=di.id
			AND s.internal_state IN ('pending_organization', 'organizing', 'pending_index', 'indexed')
		FOR UPDATE OF s
	), parent_gate AS MATERIALIZED (
		SELECT count(*) FROM locked_parents
	), reset_files AS (
		UPDATE organization_files AS f
		SET state='failed', last_error='lease expired', completed_at=NULL, updated_at=$1
		FROM candidates
		JOIN organization_batches AS expired ON expired.id=candidates.id
		CROSS JOIN parent_gate
		WHERE f.batch_id=expired.id AND expired.state='organizing' AND f.state='organizing'
		RETURNING f.id
	), claimed AS MATERIALIZED (
		UPDATE organization_batches AS b
		SET state='organizing', attempt_count=b.attempt_count+1,
			last_error='', lease_token=$3, lease_expires_at=$4, updated_at=$1
		FROM candidates
		CROSS JOIN parent_gate
		WHERE b.id=candidates.id
		RETURNING b.*
	), episodes AS (
		UPDATE subscription_episodes AS se
		SET state='organizing', updated_at=$1
		FROM claimed
		JOIN locked_parents ON locked_parents.intent_id=claimed.intent_id
		WHERE se.subscription_id=locked_parents.subscription_id
			AND se.download_intent_id=locked_parents.intent_id
			AND se.state='pending_organization'
		RETURNING se.id
	), parents AS (
		UPDATE subscriptions AS s
		SET internal_state='organizing', v2_state='R', updated_at=$1
		FROM claimed
		JOIN locked_parents ON locked_parents.intent_id=claimed.intent_id
		WHERE s.id=locked_parents.subscription_id
			AND s.current_download_intent_id=locked_parents.intent_id
			AND s.completed_at IS NULL AND s.internal_state='pending_organization'
		RETURNING s.id
	)
	SELECT claimed.id, claimed.intent_id, claimed.state, claimed.source_root,
		claimed.target_root, claimed.plan, COALESCE(claimed.last_error, ''),
		claimed.attempt_count, claimed.next_attempt_at, claimed.lease_token,
		claimed.lease_expires_at, claimed.completed_at, claimed.created_at, claimed.updated_at
	FROM claimed
	ORDER BY claimed.next_attempt_at, claimed.updated_at, claimed.id`

const claimOrganizationBatchSQL = `
	WITH candidate AS MATERIALIZED (
		SELECT b.id
		FROM organization_batches AS b
		JOIN download_intents AS di ON di.id=b.intent_id AND di.state='completed'
		LEFT JOIN subscriptions AS s ON s.id=di.subscription_id AND s.username=di.username
		WHERE b.id=$1
			AND (
				b.state IN ('planned', 'failed')
				OR (b.state='organizing' AND b.lease_expires_at <= $2)
			)
			AND (b.state='organizing' OR b.next_attempt_at <= $2)
			AND b.attempt_count < $5
			AND (di.subscription_id IS NULL OR s.id IS NOT NULL)
		FOR UPDATE OF b SKIP LOCKED
	), locked_parents AS MATERIALIZED (
		SELECT s.id AS subscription_id, di.id AS intent_id
		FROM subscriptions AS s
		JOIN download_intents AS di
			ON di.subscription_id=s.id AND di.username=s.username
		JOIN organization_batches AS b ON b.intent_id=di.id
		JOIN candidate ON candidate.id=b.id
		WHERE s.completed_at IS NULL AND s.current_download_intent_id=di.id
			AND s.internal_state IN ('pending_organization', 'organizing', 'pending_index', 'indexed')
		FOR UPDATE OF s
	), parent_gate AS MATERIALIZED (
		SELECT count(*) FROM locked_parents
	), reset_files AS (
		UPDATE organization_files AS f
		SET state='failed', last_error='lease expired', completed_at=NULL, updated_at=$2
		FROM candidate
		JOIN organization_batches AS expired ON expired.id=candidate.id
		CROSS JOIN parent_gate
		WHERE f.batch_id=expired.id AND expired.state='organizing' AND f.state='organizing'
		RETURNING f.id
	), claimed AS MATERIALIZED (
		UPDATE organization_batches AS b
		SET state='organizing', attempt_count=b.attempt_count+1,
			last_error='', lease_token=$3, lease_expires_at=$4, updated_at=$2
		FROM candidate
		CROSS JOIN parent_gate
		WHERE b.id=candidate.id
		RETURNING b.*
	), episodes AS (
		UPDATE subscription_episodes AS se
		SET state='organizing', updated_at=$2
		FROM claimed
		JOIN locked_parents ON locked_parents.intent_id=claimed.intent_id
		WHERE se.subscription_id=locked_parents.subscription_id
			AND se.download_intent_id=locked_parents.intent_id
			AND se.state='pending_organization'
		RETURNING se.id
	), parents AS (
		UPDATE subscriptions AS s
		SET internal_state='organizing', v2_state='R', updated_at=$2
		FROM claimed
		JOIN locked_parents ON locked_parents.intent_id=claimed.intent_id
		WHERE s.id=locked_parents.subscription_id
			AND s.current_download_intent_id=locked_parents.intent_id
			AND s.completed_at IS NULL AND s.internal_state='pending_organization'
		RETURNING s.id
	)
	SELECT claimed.id, claimed.intent_id, claimed.state, claimed.source_root,
		claimed.target_root, claimed.plan, COALESCE(claimed.last_error, ''),
		claimed.attempt_count, claimed.next_attempt_at, claimed.lease_token,
		claimed.lease_expires_at, claimed.completed_at, claimed.created_at, claimed.updated_at
	FROM claimed`
