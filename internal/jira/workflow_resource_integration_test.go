package jira_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

func init() {
	resource.AddTestSweepers("atlassian_jira_workflow", &resource.Sweeper{
		Name:         "atlassian_jira_workflow",
		Dependencies: []string{"atlassian_jira_workflow_scheme"},
		F:            sweepWorkflows,
	})
}

// sweepWorkflows deletes tf-acc-test-* workflows via the versioned bulk-get.
func sweepWorkflows(_ string) error {
	client, err := testutil.SweepClient()
	if err != nil {
		return fmt.Errorf("getting sweep client: %w", err)
	}
	ctx := context.Background()

	// Bulk get has no "list all" form; page the legacy search for ids/names,
	// which is still served for reads, and delete by entity id.
	allValues, err := client.GetAllPages(ctx, "/rest/api/3/workflow/search")
	if err != nil {
		return fmt.Errorf("listing workflows for sweep: %w", err)
	}
	for _, raw := range allValues {
		var wf struct {
			ID struct {
				EntityID string `json:"entityId"`
				Name     string `json:"name"`
			} `json:"id"`
		}
		if err := json.Unmarshal(raw, &wf); err != nil || !strings.HasPrefix(wf.ID.Name, "tf-acc-test-") {
			continue
		}
		delPath := fmt.Sprintf("/rest/api/3/workflow/%s", atlassian.PathEscape(wf.ID.EntityID))
		if _, delErr := client.DeleteWithStatus(ctx, delPath); delErr != nil {
			fmt.Printf("[WARN] Failed to delete workflow %q (%s): %s\n", wf.ID.Name, wf.ID.EntityID, delErr)
		}
	}
	return nil
}

// TestIntegrationWorkflowResource_full creates three global statuses and a
// workflow with a restricted, assigning transition, updates a rule in place
// and imports — against a real developer site (TF_ACC=1 + ATLASSIAN_*).
func TestIntegrationWorkflowResource_full(t *testing.T) {
	testutil.SkipIfNoAcc(t)
	rName := acctest.RandomWithPrefix("tf-acc-test")

	config := func(group string) string {
		return fmt.Sprintf(`
resource "atlassian_jira_status" "draft" {
  name            = "%[1]s-draft"
  status_category = "TODO"
}
resource "atlassian_jira_status" "review" {
  name            = "%[1]s-review"
  status_category = "IN_PROGRESS"
}
resource "atlassian_jira_status" "done" {
  name            = "%[1]s-done"
  status_category = "DONE"
}
resource "atlassian_jira_group" "reviewers" {
  name = "%[1]s-%[2]s"
}
resource "atlassian_jira_workflow" "test" {
  name        = %[1]q
  description = "Integration test workflow (run %[3]s)"
  statuses = [
    { status_id = atlassian_jira_status.draft.id },
    { status_id = atlassian_jira_status.review.id },
    { status_id = atlassian_jira_status.done.id },
  ]
  transitions = [
    {
      name            = "Request review"
      from            = [atlassian_jira_status.draft.id]
      to              = atlassian_jira_status.review.id
      allowed_groups  = [atlassian_jira_group.reviewers.group_id]
      assign          = { type = "to-reporter" }
      required_fields = ["description"]
    },
    {
      name                 = "Approve"
      from                 = [atlassian_jira_status.review.id]
      to                   = atlassian_jira_status.done.id
      separation_of_duties = [{ from = atlassian_jira_status.draft.id, to = atlassian_jira_status.review.id }]
    },
    {
      name = "Reopen"
      type = "GLOBAL"
      to   = atlassian_jira_status.draft.id
    },
  ]
}
`, rName, group, testutil.RunID())
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy:             testCheckWorkflowDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config("reviewers"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "name", rName),
					resource.TestCheckResourceAttrSet("atlassian_jira_workflow.test", "id"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "statuses.#", "3"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.#", "3"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.0.allowed_groups.#", "1"),
				),
			},
			{
				Config:   config("reviewers"),
				PlanOnly: true, // no drift after a fresh read
			},
			{
				Config: config("approvers"), // new group → in-place update of the restrict rule
				Check:  resource.TestCheckResourceAttrSet("atlassian_jira_workflow.test", "version"),
			},
			{
				ResourceName:      "atlassian_jira_workflow.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestIntegrationWorkflowDataSource_basic(t *testing.T) {
	testutil.SkipIfNoAcc(t)
	rName := acctest.RandomWithPrefix("tf-acc-test")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy:             testCheckWorkflowDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "atlassian_jira_status" "only" {
  name            = "%[1]s-status"
  status_category = "TODO"
}
resource "atlassian_jira_workflow" "test" {
  name     = %[1]q
  statuses = [{ status_id = atlassian_jira_status.only.id }]
  transitions = []
}
data "atlassian_jira_workflow" "test" {
  name = atlassian_jira_workflow.test.name
}
`, rName),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.atlassian_jira_workflow.test", "name", rName),
					resource.TestCheckResourceAttrSet("data.atlassian_jira_workflow.test", "id"),
				),
			},
		},
	})
}

func testCheckWorkflowDestroyed(s *terraform.State) error {
	client, err := testutil.SweepClient()
	if err != nil {
		return fmt.Errorf("creating client for destroy check: %w", err)
	}
	ctx := context.Background()
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "atlassian_jira_workflow" {
			continue
		}
		var out struct {
			Workflows []struct {
				ID string `json:"id"`
			} `json:"workflows"`
		}
		body := map[string][]string{"workflowIds": {rs.Primary.ID}}
		if err := client.Post(ctx, "/rest/api/3/workflows", body, &out); err != nil {
			return fmt.Errorf("error checking workflow %s destruction: %w", rs.Primary.ID, err)
		}
		if len(out.Workflows) > 0 {
			return fmt.Errorf("workflow %s still exists", rs.Primary.ID)
		}
	}
	return nil
}
