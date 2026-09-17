resource "atlassian_jira_screen_tab" "example" {
  screen_id = atlassian_jira_screen.example.id
  name      = "Example Tab"
}

# Jira gives every screen a default tab; position = 0 puts this tab (and its fields) ahead of it in the issue view.
resource "atlassian_jira_screen_tab" "first" {
  screen_id = atlassian_jira_screen.example.id
  name      = "기재사항"
  position  = 0
}
