package app

import (
	"context"
	"testing"
)

func TestPromoteProject(t *testing.T) {
	ctx := context.Background()
	a, st, _ := newFreshApp(t, "")
	a.ScratchDir = t.TempDir()
	id, err := a.CreateScratchThread(ctx, "fresh-test", "", "sketch a parser")
	if err != nil {
		t.Fatal(err)
	}
	th, _ := st.Thread(ctx, id)
	if err := a.PromoteProject(ctx, th.ProjectID, "  "); err == nil {
		t.Fatal("took an empty name")
	}
	if err := a.PromoteProject(ctx, th.ProjectID, "parser"); err != nil {
		t.Fatal(err)
	}
	p, _ := st.Project(ctx, th.ProjectID)
	if p.Scratch || p.Name != "parser" {
		t.Fatalf("project %+v", p)
	}
	if err := a.PromoteProject(ctx, th.ProjectID, "again"); err == nil {
		t.Fatal("promoted a project twice")
	}
	if err := a.PromoteProject(ctx, "p1", "x"); err == nil {
		t.Fatal("promoted a project that was never scratch")
	}
}
