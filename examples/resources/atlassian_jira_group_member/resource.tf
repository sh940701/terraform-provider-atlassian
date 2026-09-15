resource "atlassian_jira_group" "reviewers" {
  name = "change-reviewers"
}

resource "atlassian_jira_group_member" "reviewer" {
  group_id   = atlassian_jira_group.reviewers.group_id
  account_id = "5b10ac8d82e05b22cc7d4ef5"
}
