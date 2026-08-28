-- 110_sending_budget_ledger.sql
-- Expand-only account controls, provider-operation/budget ledger, and notice outbox.

-- Freeze the exact pre-migration legacy River inventory before any per-kind
-- statement runs. This snapshot deliberately takes no River row locks: source
-- deletion paths lock account-owned rows before cancelling jobs, so attribution
-- must finish locking sources before it locks this fixed set of jobs. Jobs
-- committed after this statement remain old-format for the B6/B7 resolver.
DO $$
BEGIN
    IF to_regclass('public.river_job') IS NULL THEN
        RETURN;
    END IF;

    CREATE TEMP TABLE sending_protection_legacy_job_targets (
        job_id BIGINT PRIMARY KEY,
        captured_kind TEXT NOT NULL,
        captured_args JSONB
    ) ON COMMIT DROP;

    EXECUTE $sql$
        INSERT INTO sending_protection_legacy_job_targets
            (job_id, captured_kind, captured_args)
        SELECT job.id, job.kind::text, job.args
        FROM river_job AS job
        WHERE job.kind IN ('outbound_send', 'hitl_notify', 'webhook_notify')
          AND NOT COALESCE(job.args ? 'operation_ref', false)
    $sql$;
END
$$;

CREATE TABLE IF NOT EXISTS account_sending_controls (
    user_id TEXT PRIMARY KEY,
    state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'paused')),
    reason TEXT NOT NULL DEFAULT '',
    actor TEXT NOT NULL DEFAULT '',
    outcome_epoch BIGINT NOT NULL DEFAULT 1 CHECK (outcome_epoch > 0),
    ses_tenant_name TEXT NOT NULL DEFAULT '',
    ses_tenant_ready BOOLEAN NOT NULL DEFAULT false,
    ses_tenant_ready_at TIMESTAMPTZ,
    last_resumed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ses_tenant_ready OR ses_tenant_ready_at IS NULL)
);

INSERT INTO account_sending_controls (user_id)
SELECT id FROM users
ON CONFLICT (user_id) DO NOTHING;

CREATE INDEX IF NOT EXISTS account_sending_controls_active_scan_idx
    ON account_sending_controls (updated_at, user_id)
    WHERE state = 'active';

CREATE TABLE IF NOT EXISTS account_sending_control_events (
    id TEXT PRIMARY KEY,
    account_ref TEXT NOT NULL CHECK (btrim(account_ref) <> ''),
    old_state TEXT NOT NULL CHECK (old_state IN ('active', 'paused')),
    new_state TEXT NOT NULL CHECK (new_state IN ('active', 'paused')),
    reason TEXT NOT NULL CHECK (btrim(reason) <> ''),
    actor TEXT NOT NULL CHECK (btrim(actor) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at)
);

CREATE TABLE IF NOT EXISTS sending_provider_operations (
    operation_id TEXT PRIMARY KEY CHECK (btrim(operation_id) <> ''),
    source_account_ref TEXT,
    policy_subject_ref TEXT NOT NULL CHECK (btrim(policy_subject_ref) <> ''),
    purpose TEXT NOT NULL CHECK (purpose IN (
        'customer_message', 'customer_notification', 'critical_operational',
        'violation_operational', 'public_feedback_notification', 'trusted_system'
    )),
    shared_reputation BOOLEAN NOT NULL DEFAULT false,
    current_attempt INTEGER NOT NULL DEFAULT 1 CHECK (current_attempt > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS sending_budget_counters (
    scope TEXT NOT NULL CHECK (scope IN (
        'account_daily', 'account_shared_daily', 'global_probation',
        'global_all', 'global_critical', 'global_violation'
    )),
    scope_id TEXT NOT NULL CHECK (btrim(scope_id) <> ''),
    day DATE NOT NULL,
    reserved_count INTEGER NOT NULL DEFAULT 0 CHECK (reserved_count >= 0),
    confirmed_count INTEGER NOT NULL DEFAULT 0
        CHECK (confirmed_count >= 0 AND confirmed_count <= reserved_count),
    daily_limit INTEGER NOT NULL CHECK (daily_limit > 0),
    PRIMARY KEY (scope, scope_id, day)
);

CREATE TABLE IF NOT EXISTS sending_budget_reservations (
    operation_id TEXT NOT NULL CHECK (btrim(operation_id) <> ''),
    submission_attempt INTEGER NOT NULL CHECK (submission_attempt > 0),
    source_account_ref TEXT,
    policy_subject_ref TEXT NOT NULL CHECK (btrim(policy_subject_ref) <> ''),
    purpose TEXT NOT NULL CHECK (purpose IN (
        'customer_message', 'customer_notification', 'critical_operational',
        'violation_operational', 'public_feedback_notification', 'trusted_system'
    )),
    day DATE NOT NULL,
    units INTEGER NOT NULL CHECK (units > 0),
    probation BOOLEAN NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('reserved', 'confirmed', 'released')),
    call_state TEXT NOT NULL DEFAULT 'none'
        CHECK (call_state IN ('none', 'authorized', 'started')),
    authorization_nonce TEXT,
    notice_recipient_version INTEGER
        CHECK (notice_recipient_version IS NULL OR notice_recipient_version > 0),
    notice_recipient_commitment BYTEA,
    provider_call_started_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + interval '30 days',
    CHECK ((state = 'confirmed') = (call_state IN ('authorized', 'started'))),
    CHECK ((call_state = 'none') = (authorization_nonce IS NULL)),
    CHECK ((notice_recipient_version IS NULL) = (notice_recipient_commitment IS NULL)),
    CHECK ((call_state = 'started') = (provider_call_started_at IS NOT NULL)),
    PRIMARY KEY (operation_id, submission_attempt)
);

CREATE INDEX IF NOT EXISTS sending_budget_reservations_expiry_idx
    ON sending_budget_reservations (expires_at, operation_id, submission_attempt);

CREATE TABLE IF NOT EXISTS sending_protection_notice_events (
    id TEXT PRIMARY KEY,
    account_ref TEXT,
    kind TEXT NOT NULL CHECK (kind IN ('pause', 'budget_violation', 'global_guardrail')),
    reason_code TEXT NOT NULL CHECK (reason_code IN (
        'detector_bounce', 'detector_complaint', 'ses_reputation',
        'manual', 'budget_limit', 'global_budget_exhausted'
    )),
    budget_scope TEXT,
    ledger_day DATE,
    source_event_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at),
    CHECK (
        (kind = 'pause'
            AND source_event_id IS NOT NULL
            AND budget_scope IS NULL
            AND ledger_day IS NULL
            AND account_ref IS NOT NULL
            AND reason_code NOT IN ('budget_limit', 'global_budget_exhausted'))
        OR
        (kind = 'budget_violation'
            AND source_event_id IS NULL
            AND account_ref IS NOT NULL
            AND budget_scope IN ('account_daily', 'account_shared_daily')
            AND ledger_day IS NOT NULL
            AND reason_code = 'budget_limit')
        OR
        (kind = 'global_guardrail'
            AND source_event_id IS NULL
            AND account_ref IS NULL
            AND budget_scope IN ('global_probation', 'global_all')
            AND ledger_day IS NOT NULL
            AND reason_code = 'global_budget_exhausted')
    ),
    UNIQUE (kind, source_event_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS sending_protection_notice_account_budget_uniq
    ON sending_protection_notice_events (account_ref, budget_scope, ledger_day)
    WHERE kind = 'budget_violation';

CREATE UNIQUE INDEX IF NOT EXISTS sending_protection_notice_global_guardrail_uniq
    ON sending_protection_notice_events (budget_scope, ledger_day)
    WHERE kind = 'global_guardrail';

CREATE INDEX IF NOT EXISTS sending_protection_notice_deadline_idx
    ON sending_protection_notice_events (created_at, id);

CREATE TABLE IF NOT EXISTS sending_protection_notice_deliveries (
    event_id TEXT NOT NULL REFERENCES sending_protection_notice_events(id) ON DELETE CASCADE,
    audience TEXT NOT NULL CHECK (audience IN ('owner', 'operator')),
    delivery_attempt INTEGER NOT NULL DEFAULT 0 CHECK (delivery_attempt >= 0),
    current_operation_id TEXT UNIQUE,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'sent', 'failed', 'skipped_account_deleted')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, audience)
);

CREATE INDEX IF NOT EXISTS sending_protection_notice_pending_idx
    ON sending_protection_notice_deliveries (updated_at, event_id, audience)
    WHERE state = 'pending';

INSERT INTO message_lifecycle_reason_codes (code, stage, outcome, retryable)
VALUES
    ('submission.policy_budget_expired', 'submission', 'failed', false),
    ('submission.sending_setup_expired', 'submission', 'failed', false)
ON CONFLICT (code) DO NOTHING;

DO $$
DECLARE
    drifted_code TEXT;
BEGIN
    SELECT expected.code
    INTO drifted_code
    FROM (VALUES
        ('submission.policy_budget_expired', 'submission', 'failed', false),
        ('submission.sending_setup_expired', 'submission', 'failed', false)
    ) AS expected(code, stage, outcome, retryable)
    LEFT JOIN message_lifecycle_reason_codes AS actual
      ON actual.code = expected.code
     AND actual.stage = expected.stage
     AND actual.outcome = expected.outcome
     AND actual.retryable = expected.retryable
    WHERE actual.code IS NULL
    ORDER BY expected.code
    LIMIT 1;

    IF drifted_code IS NOT NULL THEN
        RAISE EXCEPTION 'message lifecycle catalog mismatch for code %', drifted_code;
    END IF;
END
$$;

-- Snapshot migration for the exact three legacy provider-bound River job shapes.
-- Existing source fields remain in args so pre-floor rollback readers stay compatible.
DO $$
BEGIN
    IF to_regclass('public.river_job') IS NULL THEN
        RETURN;
    END IF;

    -- Resolve source identities from the fixed args snapshot without touching
    -- River rows. Agent rows are locked before their messages, matching the
    -- irreversible deletion path; message identity/direction is then rechecked
    -- under lock before any attribution is retained.
    CREATE TEMP TABLE sending_protection_message_candidates (
        job_id BIGINT PRIMARY KEY,
        captured_kind TEXT NOT NULL,
        message_id TEXT NOT NULL,
        agent_id TEXT NOT NULL
    ) ON COMMIT DROP;

    INSERT INTO sending_protection_message_candidates
        (job_id, captured_kind, message_id, agent_id)
    SELECT target.job_id, target.captured_kind,
           target.captured_args->>'message_id', message.agent_id
    FROM sending_protection_legacy_job_targets AS target
    JOIN messages AS message
      ON message.id = target.captured_args->>'message_id'
    WHERE target.captured_kind IN ('outbound_send', 'hitl_notify')
      AND COALESCE(target.captured_args->>'message_id', '') <> '';

    CREATE TEMP TABLE sending_protection_locked_agents (
        agent_id TEXT PRIMARY KEY,
        user_id TEXT NOT NULL
    ) ON COMMIT DROP;

    WITH wanted_agents AS MATERIALIZED (
        SELECT DISTINCT agent_id
        FROM sending_protection_message_candidates
    ), locked_agents AS MATERIALIZED (
        SELECT agent.id, agent.user_id
        FROM wanted_agents AS wanted
        JOIN agent_identities AS agent ON agent.id = wanted.agent_id
        ORDER BY agent.id
        FOR UPDATE OF agent
    )
    INSERT INTO sending_protection_locked_agents (agent_id, user_id)
    SELECT id, user_id FROM locked_agents;

    CREATE TEMP TABLE sending_protection_valid_sources (
        job_id BIGINT PRIMARY KEY,
        operation_id TEXT NOT NULL,
        source_account_ref TEXT NOT NULL,
        policy_subject_ref TEXT NOT NULL,
        purpose TEXT NOT NULL,
        shared_reputation BOOLEAN NOT NULL,
        source_scheduled_at TIMESTAMPTZ
    ) ON COMMIT DROP;

    WITH locked_messages AS MATERIALIZED (
        SELECT candidate.job_id,
               candidate.captured_kind,
               message.id AS message_id,
               agent.user_id,
               message.sent_as,
               message.scheduled_at
        FROM sending_protection_message_candidates AS candidate
        JOIN sending_protection_locked_agents AS agent
          ON agent.agent_id = candidate.agent_id
        JOIN messages AS message
          ON message.id = candidate.message_id
         AND message.agent_id = candidate.agent_id
        WHERE message.direction = 'outbound'
          AND (
              (candidate.captured_kind = 'outbound_send'
                  AND message.sent_as IN ('relay', 'own_address'))
              OR candidate.captured_kind = 'hitl_notify'
          )
        ORDER BY message.id, candidate.job_id
        FOR UPDATE OF message
    )
    INSERT INTO sending_protection_valid_sources
        (job_id, operation_id, source_account_ref, policy_subject_ref,
         purpose, shared_reputation, source_scheduled_at)
    SELECT job_id,
           CASE captured_kind
               WHEN 'outbound_send' THEN message_id
               ELSE 'op_' || md5('hitl_notify:' || job_id::text)
           END,
           user_id,
           user_id,
           CASE captured_kind
               WHEN 'outbound_send' THEN 'customer_message'
               ELSE 'customer_notification'
           END,
           CASE captured_kind
               WHEN 'outbound_send' THEN sent_as = 'relay'
               ELSE true
           END,
           scheduled_at
    FROM locked_messages;

    -- Webhook ownership is likewise retained under its source-row lock before
    -- the corresponding River job is touched.
    WITH locked_webhooks AS MATERIALIZED (
        SELECT target.job_id, webhook.user_id
        FROM sending_protection_legacy_job_targets AS target
        JOIN webhooks AS webhook
          ON webhook.id = target.captured_args->>'webhook_id'
        WHERE target.captured_kind = 'webhook_notify'
          AND COALESCE(target.captured_args->>'webhook_id', '') <> ''
          AND target.captured_args->>'kind' IN ('warning', 'disabled')
        ORDER BY webhook.id, target.job_id
        FOR UPDATE OF webhook
    )
    INSERT INTO sending_protection_valid_sources
        (job_id, operation_id, source_account_ref, policy_subject_ref,
         purpose, shared_reputation, source_scheduled_at)
    SELECT job_id,
           'op_' || md5('webhook_notify:' || job_id::text),
           user_id,
           user_id,
           'customer_notification',
           true,
           NULL
    FROM locked_webhooks;

    -- Only after all valid sources are locked do we lock the fixed inventory.
    -- Capturing the current tuple under lock lets every later write prove that
    -- kind and args are still exactly the migration-start tuple.
    CREATE TEMP TABLE sending_protection_locked_jobs (
        job_id BIGINT PRIMARY KEY,
        current_kind TEXT NOT NULL,
        current_args JSONB,
        current_scheduled_at TIMESTAMPTZ,
        current_state TEXT NOT NULL
    ) ON COMMIT DROP;

    EXECUTE $sql$
        WITH locked_jobs AS MATERIALIZED (
            SELECT job.id, job.kind::text AS kind, job.args,
                   job.scheduled_at, job.state::text AS state
            FROM sending_protection_legacy_job_targets AS target
            JOIN river_job AS job ON job.id = target.job_id
            ORDER BY job.id
            FOR UPDATE OF job
        )
        INSERT INTO sending_protection_locked_jobs
            (job_id, current_kind, current_args, current_scheduled_at, current_state)
        SELECT id, kind, args, scheduled_at, state FROM locked_jobs
    $sql$;

    INSERT INTO sending_provider_operations
        (operation_id, source_account_ref, policy_subject_ref,
         purpose, shared_reputation, expires_at)
    SELECT DISTINCT ON (source.operation_id)
           source.operation_id,
           source.source_account_ref,
           source.policy_subject_ref,
           source.purpose,
           source.shared_reputation,
           GREATEST(
               transaction_timestamp(),
               COALESCE(source.source_scheduled_at, '-infinity'::timestamptz),
               COALESCE(job.current_scheduled_at, '-infinity'::timestamptz)
           ) + interval '30 days'
    FROM sending_protection_valid_sources AS source
    JOIN sending_protection_locked_jobs AS job ON job.job_id = source.job_id
    JOIN sending_protection_legacy_job_targets AS target ON target.job_id = job.job_id
    WHERE job.current_kind = target.captured_kind
      AND job.current_args IS NOT DISTINCT FROM target.captured_args
      AND NOT COALESCE(job.current_args ? 'operation_ref', false)
    ORDER BY source.operation_id,
             GREATEST(
                 transaction_timestamp(),
                 COALESCE(source.source_scheduled_at, '-infinity'::timestamptz),
                 COALESCE(job.current_scheduled_at, '-infinity'::timestamptz)
             ) DESC,
             source.job_id
    ON CONFLICT (operation_id) DO NOTHING;

    EXECUTE $sql$
        UPDATE river_job AS job
        SET args = job.args || jsonb_build_object(
            'operation_ref', jsonb_build_object('v', 1, 'id', source.operation_id))
        FROM sending_protection_legacy_job_targets AS target,
             sending_protection_locked_jobs AS locked,
             sending_protection_valid_sources AS source,
             sending_provider_operations AS operation
        WHERE job.id = target.job_id
          AND locked.job_id = job.id
          AND source.job_id = job.id
          AND job.kind::text = locked.current_kind
          AND job.args IS NOT DISTINCT FROM locked.current_args
          AND locked.current_kind = target.captured_kind
          AND locked.current_args IS NOT DISTINCT FROM target.captured_args
          AND NOT COALESCE(job.args ? 'operation_ref', false)
          AND operation.operation_id = source.operation_id
          AND operation.source_account_ref IS NOT DISTINCT FROM source.source_account_ref
          AND operation.policy_subject_ref = source.policy_subject_ref
          AND operation.purpose = source.purpose
          AND operation.shared_reputation = source.shared_reputation
    $sql$;

    -- A snapshot-time job without a valid immutable source cannot be authorized.
    -- Running jobs are intentionally left for the B6/B7 compatibility resolver.
    EXECUTE $sql$
        UPDATE river_job AS job
        SET state = 'discarded', finalized_at = now()
        FROM sending_protection_legacy_job_targets AS target,
             sending_protection_locked_jobs AS locked
        WHERE job.id = target.job_id
          AND locked.job_id = job.id
          AND job.kind::text = locked.current_kind
          AND job.args IS NOT DISTINCT FROM locked.current_args
          AND job.kind IN ('outbound_send', 'hitl_notify', 'webhook_notify')
          AND NOT COALESCE(job.args ? 'operation_ref', false)
          AND job.state::text IN ('available', 'retryable', 'scheduled', 'pending')
    $sql$;
END
$$;
