resource "atlassian_jira_webhook" "collector" {
  name   = "K-CARE collector"
  url    = "https://d111111abcdef8.cloudfront.net/api/audit-process/webhook/jira"
  events = ["jira:issue_created", "jira:issue_updated", "comment_created", "comment_updated"]
  jql    = "project = OPS"
  secret = var.webhook_secret # signs deliveries (X-Hub-Signature); never returned by Jira
}
