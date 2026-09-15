package jira_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
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
					// The data source has no prior order to follow, so transitions come
					// in server order (GLOBAL first); look the transition up by name.
					func(s *terraform.State) error {
						a := s.RootModule().Resources["data.atlassian_jira_workflow.test"].Primary.Attributes
						for i := 0; i < 4; i++ {
							if a[fmt.Sprintf("transitions.%d.name", i)] != "검토 완료" {
								continue
							}
							if a[fmt.Sprintf("transitions.%d.separation_of_duties.#", i)] != "1" || a[fmt.Sprintf("transitions.%d.assign.type", i)] != "to-reporter" {
								return fmt.Errorf("검토 완료 lost its rules: %v", a)
							}
							return nil
						}
						return fmt.Errorf("transition 검토 완료 not found")
					},
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
