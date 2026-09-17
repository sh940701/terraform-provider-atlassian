# A company-managed workflow for an "infrastructure change" procedure:
# who may move the issue, who gets assigned, and what must be filled in.
resource "atlassian_jira_workflow" "infra_change" {
  name        = "Infrastructure change"
  description = "Procedure document: drive:<fileId>"
  required_field_message = "«{field}» 칸을 채워 주세요" # shown by Jira when a required field is empty; {field} = display name

  statuses = [
    { status_id = atlassian_jira_status.requested.id }, # first = initial status
    { status_id = atlassian_jira_status.in_review.id },
    { status_id = atlassian_jira_status.approved.id },
    { status_id = atlassian_jira_status.done.id },
  ]

  transitions = [
    {
      name            = "Request review"
      from            = [atlassian_jira_status.requested.id]
      to              = atlassian_jira_status.in_review.id
      allowed_groups  = [atlassian_jira_group.reviewers.group_id]
      assign          = { type = "to-selected-user", account_id = "5b10ac8d82e05b22cc7d4ef5" }
      required_fields = ["description"]
    },
    {
      name                 = "Approve"
      from                 = [atlassian_jira_status.in_review.id]
      to                   = atlassian_jira_status.approved.id
      allowed_groups       = [atlassian_jira_group.approvers.group_id]
      separation_of_duties = [{ from = atlassian_jira_status.requested.id, to = atlassian_jira_status.in_review.id }]
      assign               = { type = "to-reporter" }
    },
    {
      name = "Complete"
      from = [atlassian_jira_status.approved.id]
      to   = atlassian_jira_status.done.id
    },
    {
      name = "Reopen"
      type = "GLOBAL" # from any status
      to   = atlassian_jira_status.requested.id
    },
  ]
}
