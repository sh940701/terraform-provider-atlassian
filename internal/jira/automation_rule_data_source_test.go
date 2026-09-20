package jira_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

// jsonEquivalent compares got (already interface{}-shaped) and want
// (a literal Go value) as parsed JSON, ignoring key order and Go type
// differences (e.g. json.Number vs float64) that don't reflect a real difference.
// This is shared with automation_rule_resource_test.go; moving to a separate
// file would be ideal but left inline here to keep test files self-contained.
func dataSourceJsonEquivalent(got, want interface{}) bool {
	a, errA := json.Marshal(got)
	b, errB := json.Marshal(want)
	if errA != nil || errB != nil {
		return false
	}
	var ai, bi interface{}
	if err := json.Unmarshal(a, &ai); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bi); err != nil {
		return false
	}
	return reflect.DeepEqual(ai, bi)
}

func TestAccAutomationRuleDataSource_ReadRule(t *testing.T) {
	// Reuse the automation rule mock from the resource tests
	mock := newAutomationRuleMock()
	serverURL := setupAutomationRuleMock(t, mock)

	// Seed the mock with a test rule
	// (similar to what the resource tests do, but directly in the mock)
	testRuleUUID := "test-rule-uuid"
	testRuleDoc := map[string]interface{}{
		"name":                "Test Rule",
		"description":         "A test automation rule",
		"state":               "ENABLED",
		"trigger":             json.RawMessage(`{"type": "issue_created"}`),
		"components":          json.RawMessage(`[{"type": "transition", "value": "Done"}]`),
		"ruleScopeARIs":       []interface{}{"ari:cloud:jira:abc:project/10549"},
		"canOtherRuleTrigger": true,
		"notifyOnError":       "FIRSTERROR",
		"uuid":                testRuleUUID,
	}
	mock.mu.Lock()
	mock.rules[testRuleUUID] = testRuleDoc
	mock.mu.Unlock()

	config := fmt.Sprintf(`provider "atlassian" {
  url                 = %[1]q
  automation_base_url = %[1]q
}

data "atlassian_jira_automation_rule" "test" {
  uuid = %[2]q
}
`, serverURL, testRuleUUID)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.atlassian_jira_automation_rule.test", "id", testRuleUUID),
					resource.TestCheckResourceAttr("data.atlassian_jira_automation_rule.test", "uuid", testRuleUUID),
					resource.TestCheckResourceAttr("data.atlassian_jira_automation_rule.test", "name", "Test Rule"),
					resource.TestCheckResourceAttr("data.atlassian_jira_automation_rule.test", "description", "A test automation rule"),
					resource.TestCheckResourceAttr("data.atlassian_jira_automation_rule.test", "state", "ENABLED"),
					resource.TestCheckResourceAttr("data.atlassian_jira_automation_rule.test", "can_other_rule_trigger", "true"),
					resource.TestCheckResourceAttr("data.atlassian_jira_automation_rule.test", "notify_on_error", "FIRSTERROR"),
					resource.TestCheckTypeSetElemAttr("data.atlassian_jira_automation_rule.test", "project_ids.*", "10549"),
					checkBodyJsonEquivalent(testRuleDoc),
				),
			},
		},
	})
}

func TestAccAutomationRuleDataSource_NotFound(t *testing.T) {
	mock := newAutomationRuleMock()
	serverURL := setupAutomationRuleMock(t, mock)

	config := fmt.Sprintf(`provider "atlassian" {
  url                 = %[1]q
  automation_base_url = %[1]q
}

data "atlassian_jira_automation_rule" "test" {
  uuid = "nonexistent-uuid"
}
`, serverURL)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile("not found"),
			},
		},
	})
}

// checkBodyJsonEquivalent returns a TestCheckFunc that verifies the data source's
// body attribute matches the mock's trigger+components as JSON, ignoring key order.
func checkBodyJsonEquivalent(mockDoc map[string]interface{}) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		rs := state.RootModule().Resources["data.atlassian_jira_automation_rule.test"]
		if rs == nil {
			return fmt.Errorf("data source not found in state")
		}

		gotStr, ok := rs.Primary.Attributes["body"]
		if !ok {
			return fmt.Errorf("body attribute not found in state")
		}

		// Parse the data source's body (a JSON string: {"trigger": {...}, "components": [...]})
		var gotBody map[string]interface{}
		if err := json.Unmarshal([]byte(gotStr), &gotBody); err != nil {
			return fmt.Errorf("failed to parse body: %w", err)
		}

		// Build the expected body from the mock's trigger and components
		wantBody := map[string]interface{}{
			"trigger":    mockDoc["trigger"],
			"components": mockDoc["components"],
		}

		// Compare as parsed JSON
		if !dataSourceJsonEquivalent(gotBody, wantBody) {
			gotJSON, _ := json.Marshal(gotBody)
			wantJSON, _ := json.Marshal(wantBody)
			return fmt.Errorf("body mismatch\ngot:  %s\nwant: %s", string(gotJSON), string(wantJSON))
		}
		return nil
	}
}
