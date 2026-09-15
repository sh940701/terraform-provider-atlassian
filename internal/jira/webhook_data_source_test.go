package jira_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

func TestAccWebhookDataSource_basic(t *testing.T) {
	mock := newWebhookMock()
	mock.hooks["7"] = map[string]interface{}{
		"name": "existing", "description": "", "url": "https://example.com/x", "excludeBody": false,
		"events": []interface{}{"jira:issue_updated"}, "filters": map[string]interface{}{"issue-related-events-section": "project = HANON"},
		"enabled": true, "secret": "abc",
	}
	setupWebhookMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `data "atlassian_jira_webhook" "test" { id = "7" }`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.atlassian_jira_webhook.test", "name", "existing"),
					resource.TestCheckResourceAttr("data.atlassian_jira_webhook.test", "url", "https://example.com/x"),
					resource.TestCheckResourceAttr("data.atlassian_jira_webhook.test", "jql", "project = HANON"),
					resource.TestCheckResourceAttr("data.atlassian_jira_webhook.test", "events.#", "1"),
					resource.TestCheckResourceAttr("data.atlassian_jira_webhook.test", "enabled", "true"),
					resource.TestCheckResourceAttr("data.atlassian_jira_webhook.test", "is_signed", "true"),
				),
			},
		},
	})
}
