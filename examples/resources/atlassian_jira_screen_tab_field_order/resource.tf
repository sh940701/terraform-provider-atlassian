# 탭의 칸 순서 — 나열한 칸이 그 순서로 앞에 온다. 나열하지 않은 칸은 그 뒤에 남는다.
resource "atlassian_jira_screen_tab_field_order" "details" {
  screen_id = atlassian_jira_screen.change.id
  tab_id    = atlassian_jira_screen_tab.details.id
  field_ids = [
    "summary",
    "description",
    "attachment",
    "assignee",
    atlassian_jira_custom_field.target.id,
    atlassian_jira_custom_field.reason.id,
  ]

  depends_on = [atlassian_jira_screen_tab_field.details]
}
