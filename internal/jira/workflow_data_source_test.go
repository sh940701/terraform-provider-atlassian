package jira_test

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

func TestAccWorkflowDataSource_ByName(t *testing.T) {
	mock := newWorkflowMock()
	setupWorkflowMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: workflowConfigV1 + `
data "atlassian_jira_workflow" "test" {
  name = atlassian_jira_workflow.test.name
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.atlassian_jira_workflow.test", "id", workflowFixedEntityID),
					resource.TestCheckResourceAttr("data.atlassian_jira_workflow.test", "version", "1"),
					resource.TestCheckResourceAttr("data.atlassian_jira_workflow.test", "statuses.#", "4"),
					resource.TestCheckResourceAttr("data.atlassian_jira_workflow.test", "transitions.#", "4"),
					resource.TestCheckResourceAttr("data.atlassian_jira_workflow.test", "transitions.1.separation_of_duties.#", "1"),
					resource.TestCheckResourceAttr("data.atlassian_jira_workflow.test", "transitions.1.assign.type", "to-reporter"),
				),
			},
		},
	})
}

func TestAccWorkflowDataSource_NotFound(t *testing.T) {
	mock := newWorkflowMock()
	setupWorkflowMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      `data "atlassian_jira_workflow" "test" { name = "NonExistentWorkflow" }`,
				ExpectError: regexp.MustCompile("Workflow not found"),
			},
		},
	})
}
