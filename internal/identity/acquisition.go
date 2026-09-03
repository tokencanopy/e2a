package identity

import "errors"

// AcquisitionSources is the closed answer set for the onboarding survey
// ("Where did you hear about e2a?"). It must match the CHECK constraint
// in migrations/120_users_acquisition_survey.sql exactly — the values
// are the analytics enum, so they are code, not config.
var AcquisitionSources = []string{
	"search",
	"ai_assistant",
	"github",
	"x_twitter",
	"hn_reddit",
	"content",
	"mcp_directory",
	"word_of_mouth",
	"other",
	AcquisitionSourceSkipped,
}

// AcquisitionSourceSkipped records "asked, declined". It counts as
// answered so the survey never reappears.
const AcquisitionSourceSkipped = "skipped"

// ErrAcquisitionSurveyAnswered is returned by RecordAcquisitionSurvey when
// the user already has an answer on file. The first answer is kept.
var ErrAcquisitionSurveyAnswered = errors.New("acquisition survey already answered")

// IsAcquisitionSource reports whether s is exactly one of AcquisitionSources.
func IsAcquisitionSource(s string) bool {
	for _, v := range AcquisitionSources {
		if v == s {
			return true
		}
	}
	return false
}
