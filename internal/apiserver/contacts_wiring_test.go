package apiserver_test

import (
	"testing"

	"github.com/tokencanopy/e2a/internal/apiserver"
)

// TestContactDepsAreWired guards a failure mode the httpapi handler tests are
// structurally blind to.
//
// Those tests inject fakes straight into Deps, so they pass whether or not
// BuildDeps ever connects the real store. A contacts surface that compiled,
// passed 23 handler tests, and returned 501 not_implemented for every request
// in the shipped binary is exactly what happened before this test existed.
//
// Any new resource added to Deps needs a line here, because no other test in
// the suite can catch an unwired capability.
func TestContactDepsAreWired(t *testing.T) {
	p, _ := realParams(t)
	deps := apiserver.BuildDeps(p)

	for name, wired := range map[string]bool{
		"CreateContact":               deps.CreateContact != nil,
		"GetContact":                  deps.GetContact != nil,
		"ListContacts":                deps.ListContacts != nil,
		"UpdateContact":               deps.UpdateContact != nil,
		"UpdateContactIfUnchanged":    deps.UpdateContactIfUnchanged != nil,
		"DeleteContact":               deps.DeleteContact != nil,
		"ImportContacts":              deps.ImportContacts != nil,
		"ImportContactsWithOptions":   deps.ImportContactsWithOptions != nil,
		"DeleteImportBatch":           deps.DeleteImportBatch != nil,
		"SuppressedAddresses":         deps.SuppressedAddresses != nil,
		"EffectiveSuppressions":       deps.EffectiveSuppressions != nil,
		"UpsertEngagement":            deps.UpsertEngagement != nil,
		"UpdateEngagementIfUnchanged": deps.UpdateEngagementIfUnchanged != nil,
		"GetEngagement":               deps.GetEngagement != nil,
		"ListEngagements":             deps.ListEngagements != nil,
		"DeleteEngagement":            deps.DeleteEngagement != nil,
	} {
		if !wired {
			t.Errorf("Deps.%s is nil — the endpoint would return 501 not_implemented in the real binary", name)
		}
	}
}
