package jira_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/jira"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

func init() {
	resource.AddTestSweepers("atlassian_jira_automation_rule", &resource.Sweeper{
		Name: "atlassian_jira_automation_rule",
		F:    sweepAutomationRules,
	})
}

func sweepAutomationRules(_ string) error {
	client, err := testutil.SweepClient()
	if err != nil {
		return fmt.Errorf("getting sweep client: %w", err)
	}

	ctx := context.Background()

	summaries, err := jira.ListAutomationRuleSummaries(ctx, client)
	if err != nil {
		return fmt.Errorf("listing automation rules for sweep: %w", err)
	}

	for _, rule := range summaries {
		if !strings.HasPrefix(rule.Name, "tf-acc-test-") {
			continue
		}
		if err := jira.DeleteAutomationRule(ctx, client, rule.UUID); err != nil {
			fmt.Printf("[WARN] Failed to delete automation rule %q (%s): %s\n", rule.Name, rule.UUID, err)
		}
	}

	return nil
}

func TestIntegrationAutomationRuleResource_basic(t *testing.T) {
	testutil.SkipIfNoAcc(t)

	// Real-site fixture ids. Skip rather than 400 if any is missing.
	//   ATLASSIAN_TEST_PROJECT_ID     — project the rule is scoped to
	//   ATLASSIAN_TEST_STATUS_ID      — transitioned-to status in the trigger
	//   ATLASSIAN_TEST_ISSUE_TYPE_ID  — issuetype condition operand (not a project id)
	projectID := os.Getenv("ATLASSIAN_TEST_PROJECT_ID")
	statusID := os.Getenv("ATLASSIAN_TEST_STATUS_ID")
	issueTypeID := os.Getenv("ATLASSIAN_TEST_ISSUE_TYPE_ID")
	if projectID == "" {
		t.Skip("ATLASSIAN_TEST_PROJECT_ID not set")
	}
	if statusID == "" {
		t.Skip("ATLASSIAN_TEST_STATUS_ID not set")
	}
	if issueTypeID == "" {
		t.Skip("ATLASSIAN_TEST_ISSUE_TYPE_ID not set")
	}

	rName := acctest.RandomWithPrefix("tf-acc-test")

	body := fmt.Sprintf(`jsonencode({
    trigger = {
      type = "jira.issue.event.trigger:transitioned"
      value = {
        toStatus = [
          {
            type  = "ID"
            value = "%s"
          }
        ]
      }
    }
    components = [
      {
        type = "jira.issue.condition"
        value = {
          operand = {
            type  = "ID"
            value = "%s"
          }
          operator = {
            value = "="
          }
          field = {
            value = "issuetype"
          }
        }
      }
    ]
  })`, statusID, issueTypeID)

	expectedBodyJSON := fmt.Sprintf(`{"trigger":{"type":"jira.issue.event.trigger:transitioned","value":{"toStatus":[{"type":"ID","value":%q}]}},"components":[{"type":"jira.issue.condition","value":{"operand":{"type":"ID","value":%q},"operator":{"value":"="},"field":{"value":"issuetype"}}}]}`, statusID, issueTypeID)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy:             testCheckAutomationRuleDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "atlassian_jira_automation_rule" "test" {
  name        = %q
  description = "Integration test automation rule (run %s)"
  state       = "DISABLED"
  project_ids = [%q]
  body        = %s
}
`, rName, testutil.RunID(), projectID, body),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "name", rName),
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "state", "DISABLED"),
					resource.TestCheckResourceAttrSet("atlassian_jira_automation_rule.test", "uuid"),
					resource.TestCheckResourceAttrSet("atlassian_jira_automation_rule.test", "id"),
				),
			},
			{
				ResourceName:            "atlassian_jira_automation_rule.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"body"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected 1 imported instance state, got %d", len(states))
					}
					got := states[0].Attributes["body"]
					if got == "" {
						return fmt.Errorf("imported body is empty")
					}
					var gotBody, wantBody interface{}
					if err := json.Unmarshal([]byte(got), &gotBody); err != nil {
						return fmt.Errorf("imported body is not JSON: %w", err)
					}
					if err := json.Unmarshal([]byte(expectedBodyJSON), &wantBody); err != nil {
						return fmt.Errorf("expected body is not JSON: %w", err)
					}
					// Server re-serializes body on import; strip the keys
					// normalizeBody ignores so this is JSON equality of
					// trigger/components, not string equality of the stored body.
					if !reflect.DeepEqual(stripServerNoise(gotBody), stripServerNoise(wantBody)) {
						return fmt.Errorf("imported body does not match expected trigger/components: got %s", got)
					}
					return nil
				},
			},
			{
				Config: fmt.Sprintf(`
resource "atlassian_jira_automation_rule" "test" {
  name        = %q
  description = "Integration test automation rule updated (run %s)"
  state       = "DISABLED"
  project_ids = [%q]
  body        = %s
}
`, rName+"-upd", testutil.RunID(), projectID, body),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "name", rName+"-upd"),
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "state", "DISABLED"),
				),
			},
		},
	})
}

// stripServerNoise drops the keys the Automation API adds on echo
// (id, schemaVersion, empty conditions/children) so ImportStateCheck can
// compare trigger/components as JSON rather than as stored strings.
func stripServerNoise(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(val))
		for k, child := range val {
			if k == "id" || k == "schemaVersion" {
				continue
			}
			if k == "conditions" || k == "children" {
				if arr, ok := child.([]interface{}); ok && len(arr) == 0 {
					continue
				}
			}
			out[k] = stripServerNoise(child)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(val))
		for i, child := range val {
			out[i] = stripServerNoise(child)
		}
		return out
	default:
		return val
	}
}

func testCheckAutomationRuleDestroyed(s *terraform.State) error {
	client, err := testutil.SweepClient()
	if err != nil {
		return fmt.Errorf("creating client for destroy check: %w", err)
	}
	ctx := context.Background()

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "atlassian_jira_automation_rule" {
			continue
		}

		ruleURL, err := client.AutomationURL(ctx, "/rule/"+atlassian.PathEscape(rs.Primary.ID))
		if err != nil {
			return fmt.Errorf("building automation rule URL for %s: %w", rs.Primary.ID, err)
		}

		var result interface{}
		statusCode, err := client.GetWithStatus(ctx, ruleURL, &result)
		if err != nil {
			return fmt.Errorf("error checking automation rule %s destruction: %w", rs.Primary.ID, err)
		}
		if statusCode != http.StatusNotFound {
			return fmt.Errorf("automation rule %s still exists (status %d)", rs.Primary.ID, statusCode)
		}
	}
	return nil
}
