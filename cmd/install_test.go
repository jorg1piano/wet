package cmd

import (
	"strings"
	"testing"
)

// TestEmbeddedSkillTemplate smoke-checks the //go:embed directive in
// install.go — confirms the template file made it into the binary, starts
// with the expected Claude Code frontmatter, and still contains a mention
// of `project_name` (the most distinctive schema keyword, so it catches
// accidental truncation or wrong-file embeds).
func TestEmbeddedSkillTemplate(t *testing.T) {
	if wetSkillTemplate == "" {
		t.Fatal("wetSkillTemplate is empty — //go:embed did not capture the file")
	}
	if !strings.HasPrefix(wetSkillTemplate, "---\nname: wet") {
		t.Errorf("wetSkillTemplate missing expected frontmatter prefix; got first 80 bytes: %q",
			wetSkillTemplate[:min(80, len(wetSkillTemplate))])
	}
	if !strings.Contains(wetSkillTemplate, "project_name") {
		t.Error("wetSkillTemplate does not contain 'project_name' — schema body may be missing")
	}
}
