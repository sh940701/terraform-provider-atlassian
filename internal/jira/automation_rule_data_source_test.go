package jira_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

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
