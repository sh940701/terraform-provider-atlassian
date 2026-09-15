data "atlassian_jira_workflow" "infra_change" {
  name = "Infrastructure change"
}

output "review_transition_groups" {
  value = data.atlassian_jira_workflow.infra_change.transitions[0].allowed_groups
}
