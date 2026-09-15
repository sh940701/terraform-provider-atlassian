package jira_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

func init() {
	resource.AddTestSweepers("atlassian_jira_webhook", &resource.Sweeper{
		Name: "atlassian_jira_webhook",
		F:    sweepWebhooks,
	})
}

// sweepWebhooks deletes administrator webhooks named tf-acc-test-*.
func sweepWebhooks(_ string) error {
	client, err := testutil.SweepClient()
	if err != nil {
		return fmt.Errorf("getting sweep client: %w", err)
	}
	ctx := context.Background()

	var hooks []struct {
		Name string `json:"name"`
		Self string `json:"self"`
	}
	if err := client.Get(ctx, "/rest/webhooks/1.0/webhook", &hooks); err != nil {
		return fmt.Errorf("listing webhooks for sweep: %w", err)
	}
	for _, h := range hooks {
		if !strings.HasPrefix(h.Name, "tf-acc-test-") {
			continue
		}
		id := h.Self[strings.LastIndex(h.Self, "/")+1:]
		if _, delErr := client.DeleteWithStatus(ctx, "/rest/webhooks/1.0/webhook/"+atlassian.PathEscape(id)); delErr != nil {
			fmt.Printf("[WARN] Failed to delete webhook %q (%s): %s\n", h.Name, id, delErr)
		}
	}
	return nil
}

func TestIntegrationWebhookResource_basic(t *testing.T) {
	testutil.SkipIfNoAcc(t)
	rName := acctest.RandomWithPrefix("tf-acc-test")

	config := func(name, jql string) string {
		return fmt.Sprintf(`
resource "atlassian_jira_webhook" "test" {
  name   = %q
  url    = "https://example.com/hooks/%s"
  events = ["jira:issue_created", "jira:issue_updated"]
  jql    = %q
  secret = "tf-acc-secret-%s"
}
`, name, testutil.RunID(), jql, testutil.RunID())
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy:             testCheckWebhookDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config(rName, "project = DEV"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "name", rName),
					resource.TestCheckResourceAttrSet("atlassian_jira_webhook.test", "id"),
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "enabled", "true"),
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "is_signed", "true"),
				),
			},
			{
				Config: config(rName, "project = DEV AND issuetype = Task"),
				Check:  resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "jql", "project = DEV AND issuetype = Task"),
			},
			{
				ResourceName:            "atlassian_jira_webhook.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"},
			},
		},
	})
}

func testCheckWebhookDestroyed(s *terraform.State) error {
	client, err := testutil.SweepClient()
	if err != nil {
		return fmt.Errorf("creating client for destroy check: %w", err)
	}
	ctx := context.Background()
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "atlassian_jira_webhook" {
			continue
		}
		var out map[string]interface{}
		status, err := client.GetWithStatus(ctx, "/rest/webhooks/1.0/webhook/"+atlassian.PathEscape(rs.Primary.ID), &out)
		if err != nil {
			return fmt.Errorf("error checking webhook %s destruction: %w", rs.Primary.ID, err)
		}
		if status != http.StatusNotFound {
			return fmt.Errorf("webhook %s still exists", rs.Primary.ID)
		}
	}
	return nil
}

// TestIntegrationGroupMemberResource_basic needs a site-admin token: it adds
// the calling user (from /rest/api/3/myself) to a fresh group and removes it.
func TestIntegrationGroupMemberResource_basic(t *testing.T) {
	testutil.SkipIfNoAcc(t)
	rName := acctest.RandomWithPrefix("tf-acc-test")

	client, err := atlassian.NewClient(atlassian.ClientConfig{})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	var me struct {
		AccountID string `json:"accountId"`
	}
	if err := client.Get(context.Background(), "/rest/api/3/myself", &me); err != nil {
		t.Fatalf("myself: %v", err)
	}

	config := fmt.Sprintf(`
resource "atlassian_jira_group" "test" {
  name = %q
}
resource "atlassian_jira_group_member" "test" {
  group_id   = atlassian_jira_group.test.group_id
  account_id = %q
}
data "atlassian_jira_group_members" "test" {
  group_id   = atlassian_jira_group.test.group_id
  depends_on = [atlassian_jira_group_member.test]
}
`, rName, me.AccountID)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_group_member.test", "account_id", me.AccountID),
					resource.TestCheckResourceAttr("data.atlassian_jira_group_members.test", "account_ids.#", "1"),
				),
			},
			{
				ResourceName:      "atlassian_jira_group_member.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}
