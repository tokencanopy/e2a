-- Onboarding acquisition survey ("Where did you hear about e2a?").
-- Write-once per user; NULL = not yet asked, 'skipped' = asked and
-- declined. The dashboard only shows the survey when the server's
-- onboarding_survey.enabled flag is on, so these columns stay NULL on
-- deployments that never enable it.
ALTER TABLE users ADD COLUMN IF NOT EXISTS acquisition_source TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS acquisition_detail TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS acquisition_answered_at TIMESTAMPTZ;

DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_acquisition_source_check
        CHECK (acquisition_source IS NULL OR acquisition_source IN (
            'search', 'ai_assistant', 'github', 'x_twitter', 'hn_reddit',
            'content', 'mcp_directory', 'word_of_mouth', 'other', 'skipped'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- Source and timestamp are set together or not at all.
DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_acquisition_answered_check
        CHECK ((acquisition_source IS NULL) = (acquisition_answered_at IS NULL));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;
