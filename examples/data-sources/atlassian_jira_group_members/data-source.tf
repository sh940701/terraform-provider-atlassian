data "atlassian_jira_group_members" "reviewers" {
  group_id = atlassian_jira_group.reviewers.group_id
}
