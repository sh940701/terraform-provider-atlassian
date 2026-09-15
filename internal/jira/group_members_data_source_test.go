package jira_test

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

func TestAccGroupMembersDataSource_basic(t *testing.T) {
	mock := &groupMemberMock{members: map[string][]string{gmGroupID: {"a1", "a2", "a3"}}, pageSize: 2}
	setupGroupMemberMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`data "atlassian_jira_group_members" "test" { group_id = %q }`, gmGroupID),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.atlassian_jira_group_members.test", "account_ids.#", "3"),
					resource.TestCheckResourceAttr("data.atlassian_jira_group_members.test", "account_ids.0", "a1"),
					resource.TestCheckResourceAttr("data.atlassian_jira_group_members.test", "account_ids.2", "a3"),
				),
			},
		},
	})
}
