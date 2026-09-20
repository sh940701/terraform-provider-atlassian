package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
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

	// List automation rules via the Automation Rule Management API.
	automationURL, err := client.AutomationURL(ctx, "/rules")
	if err != nil {
		return fmt.Errorf("building automation rules URL: %w", err)
	}

	var result struct {
		Rules []struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"rules"`
	}
	if err := client.Get(ctx, automationURL, &result); err != nil {
		return fmt.Errorf("listing automation rules for sweep: %w", err)
	}

	for _, rule := range result.Rules {
		if !strings.HasPrefix(rule.Name, "tf-acc-test-") {
			continue
		}

		// Disable then delete, matching the resource's Delete sequence.
		ruleURL, err := client.AutomationURL(ctx, "/rule/"+atlassian.PathEscape(rule.UUID))
		if err != nil {
			fmt.Printf("[WARN] building rule URL for %q (%s): %s\n", rule.Name, rule.UUID, err)
			continue
		}

		stateURL := ruleURL + "/state"
		_, _ = client.PutWithStatus(ctx, stateURL, map[string]string{"state": "DISABLED"}, nil)

		_, delErr := client.DeleteWithStatus(ctx, ruleURL)
		if delErr != nil {
			fmt.Printf("[WARN] Failed to delete automation rule %q (%s): %s\n", rule.Name, rule.UUID, delErr)
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
					var parsed interface{}
					if err := json.Unmarshal([]byte(got), &parsed); err != nil {
						return fmt.Errorf("imported body is not JSON: %w", err)
					}
					equal, err := bodyEqual(got, expectedBodyJSON)
					if err != nil {
						return fmt.Errorf("comparing imported body: %w", err)
					}
					if !equal {
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
