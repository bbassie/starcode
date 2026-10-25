package app

import (
	"context"
	"testing"
	"time"

	"starcode/internal/domain"
)

func TestScheduleOnThread(t *testing.T) {
	ctx := context.Background()
	a, id := childApp(t)
	sid, err := a.SetSchedule(ctx, domain.ScheduleSet{ThreadID: id, Prompt: "later", At: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	a.runSchedules(ctx, time.Now())
	if items, _ := a.Store.Items(ctx, id); len(items) != 0 {
		t.Fatal("ran early")
	}
	a.runSchedules(ctx, time.Now().Add(2*time.Hour))
	items, _ := a.Store.Items(ctx, id)
	if len(items) == 0 || items[0].Body != "later" || promptMeta(items[0]).Auto != "schedule" {
		t.Fatalf("items %+v", items)
	}
	if _, err := a.Store.Schedule(ctx, sid); err == nil {
		t.Fatal("a one-off outlived its run")
	}
	waitIdle(t, a.Store, id)
	// Run now ends a one-off.
	sid, _ = a.SetSchedule(ctx, domain.ScheduleSet{ThreadID: id, Prompt: "now please", At: time.Now().Add(time.Hour)})
	if err := a.RunSchedule(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.Schedule(ctx, sid); err == nil {
		t.Fatal("a one-off run by hand stays scheduled")
	}
	waitIdle(t, a.Store, id)
	if _, err := a.SetSchedule(ctx, domain.ScheduleSet{ThreadID: id, Prompt: "x", At: time.Now().Add(-time.Minute)}); err == nil {
		t.Fatal("a time in the past was taken")
	}
}

func TestCronScheduleOpensThreads(t *testing.T) {
	ctx := context.Background()
	a, first := childApp(t)
	th, _ := a.Store.Thread(ctx, first)
	sid, err := a.SetSchedule(ctx, domain.ScheduleSet{ProjectID: th.ProjectID, Prompt: "daily check", Title: "Daily", Cron: "0 9 * * *", TZ: "Europe/Amsterdam", Agent: "fake", Mode: "yolo"})
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := a.Store.Schedule(ctx, sid)
	if sc.Next.In(time.UTC).Hour() != 8 && sc.Next.In(time.UTC).Hour() != 7 {
		t.Fatalf("next %v", sc.Next)
	}
	a.runSchedules(ctx, sc.Next.Add(time.Minute))
	after, _ := a.Store.Schedule(ctx, sid)
	if !after.Next.After(sc.Next) || after.LastThread == "" || after.LastError != "" {
		t.Fatalf("after %+v", after)
	}
	run, _ := a.Store.Thread(ctx, after.LastThread)
	if run.PermissionMode != "yolo" || run.Title[:5] != "Daily" {
		t.Fatalf("thread %+v", run)
	}
	waitIdle(t, a.Store, run.ID)
	if err := a.PauseSchedule(ctx, sid, true); err != nil {
		t.Fatal(err)
	}
	a.runSchedules(ctx, after.Next.Add(time.Minute))
	if again, _ := a.Store.Schedule(ctx, sid); again.LastThread != after.LastThread {
		t.Fatal("a paused schedule ran")
	}
	if err := a.RemoveSchedule(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if n, err := a.Store.Replay(ctx); err != nil || n == 0 {
		t.Fatalf("replay %d %v", n, err)
	}
}
