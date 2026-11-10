package resources

import (
	"slices"
)

// AccessFailure reports only known policy facts. It never guesses an entitlement
// failure when the qualification or rights provider is unavailable.
type AccessFailure struct {
	Reason      string   `json:"access_reason"`
	Missing     []string `json:"missing_conditions,omitempty"`
	Unavailable []string `json:"unavailable_conditions,omitempty"`
	RuleVersion string   `json:"rule_version,omitempty"`
}

func (e *AccessFailure) Error() string { return e.Unwrap().Error() }
func (e *AccessFailure) Unwrap() error {
	if e.Reason == "service_unavailable" || len(e.Unavailable) > 0 {
		return ErrUnavailable
	}
	return ErrDenied
}
func (e *AccessFailure) valid() bool {
	if !slices.Contains([]string{"requirements_missing", "service_unavailable", "purchase_unavailable", "resource_unavailable", "permission_denied"}, e.Reason) || len(e.RuleVersion) > 128 || len(e.Missing)+len(e.Unavailable) > 6 {
		return false
	}
	seen := map[string]bool{}
	for _, list := range [][]string{e.Missing, e.Unavailable} {
		for _, key := range list {
			if !slices.Contains([]string{"content_access", "campus_eligibility", "phone_bound", "identity_verified", "course_enrollment", "resource_entitlement"}, key) || seen[key] {
				return false
			}
			seen[key] = true
		}
	}
	return true
}
