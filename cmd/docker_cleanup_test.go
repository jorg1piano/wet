package cmd

import "testing"

func TestPlanDockerCleanupKeepsActiveAndPoolProjects(t *testing.T) {
	containers := []dockerComposeContainer{
		{ID: "1", Name: "active-web", Project: "app-active"},
		{ID: "2", Name: "pool-db", Project: "app-slot-1"},
		{ID: "3", Name: "stale-web", Project: "app-stale"},
		{ID: "4", Name: "stale-db", Project: "app-stale"},
	}
	keep := map[string]bool{"app-active": true, "app-slot-1": true}
	got := planDockerCleanup(&WetConfig{ProjectName: "app"}, containers, keep)
	if len(got) != 1 {
		t.Fatalf("planned %d projects, want 1: %#v", len(got), got)
	}
	if got[0].Project != "app-stale" || len(got[0].Containers) != 2 {
		t.Fatalf("planned %#v, want only the two app-stale containers", got[0])
	}
}

func TestPlanDockerCleanupIgnoresOtherRepos(t *testing.T) {
	containers := []dockerComposeContainer{
		{ID: "1", Name: "sendi-db", Project: "sendi-slot-1"},
		{ID: "2", Name: "template-db", Project: "template_framework-slot-1"},
	}
	got := planDockerCleanup(&WetConfig{ProjectName: "sendi"}, containers, nil)
	if len(got) != 1 || got[0].Project != "sendi-slot-1" {
		t.Fatalf("planned %#v, want only the sendi project", got)
	}
}

func TestPathInsideIsInclusive(t *testing.T) {
	dir := t.TempDir()
	if !pathInside(dir, dir) {
		t.Fatal("pathInside should treat a checkout root as inside itself")
	}
	if !pathInside(dir+"/sub/dir", dir) {
		t.Fatal("pathInside should include descendants")
	}
	if pathInside(dir+"-other", dir) {
		t.Fatal("pathInside should not use raw prefix matching")
	}
}
