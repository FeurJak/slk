package config

import "testing"

func TestAgentConfig_Decodes(t *testing.T) {
	cfg := loadConfigString(t, `
[agent]
github_org = "acme"
linear_workspace = "acmeco"
linear_teams = ["EXP", "fro"]
`)
	a := cfg.Agent
	if a.GitHubOrg != "acme" || a.LinearWorkspace != "acmeco" || len(a.LinearTeams) != 2 || a.LinearTeams[1] != "fro" {
		t.Errorf("Agent = %+v", a)
	}
}

func TestAgentConfig_AbsentIsZero(t *testing.T) {
	cfg := loadConfigString(t, "")
	if cfg.Agent.GitHubOrg != "" || cfg.Agent.LinearWorkspace != "" || cfg.Agent.LinearTeams != nil {
		t.Errorf("Agent = %+v, want zero", cfg.Agent)
	}
}
