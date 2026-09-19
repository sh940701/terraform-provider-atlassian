resource "atlassian_jira_automation_rule" "ops_ticket_watcher" {
  name        = "K-CARE ticket watcher"
  description = "Notifies #ops when a P1 issue is created."
  project_ids = ["10549"]

  body = jsonencode({
    trigger = {
      component = "TRIGGER"
      type      = "jira.manual.trigger.trigger"
    }
    components = [
      {
        component = "ACTION"
        type      = "jira.issue.assign"
        value = {
          assignee = "current-user"
        }
      },
    ]
  })
}
