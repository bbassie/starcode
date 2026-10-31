package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// cliDo calls the command line's API on the agent handler.
func cliDo(t *testing.T, s *Server, method, path, body string, out any) int {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	s.AgentHandler().ServeHTTP(w, r)
	if out != nil && w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v\n%s", method, path, err, w.Body)
		}
	}
	return w.Code
}

func TestCLIStartWaitShow(t *testing.T) {
	s, _ := agentServer(t)
	p, _ := s.App.Store.Project(context.Background(), "p1")
	for _, ref := range []string{"p1", "P", p.Path, p.Path + "/sub/dir"} {
		var th CLIThread
		body, _ := json.Marshal(CLIStart{Project: ref, Prompt: "hi from " + ref, Agent: "fake", Mode: "yolo"})
		if code := cliDo(t, s, "POST", "/cli/threads", string(body), &th); code != 200 || th.ID == "" || th.Project != "p" {
			t.Fatalf("start in %q: %d %+v", ref, code, th)
		}
	}
	if code := cliDo(t, s, "POST", "/cli/threads", `{"project":"elsewhere","prompt":"x"}`, nil); code != 400 {
		t.Fatalf("unknown project: %d", code)
	}
	var ts []CLIThread
	cliDo(t, s, "GET", "/cli/threads?project=p1", "", &ts)
	if len(ts) != 5 { // the four above and the agentServer's own
		t.Fatalf("list: %+v", ts)
	}
	id := ts[0].ID
	var w struct{ Done bool }
	if cliDo(t, s, "POST", "/cli/threads/"+id+"/wait?timeout=10", "", &w); !w.Done {
		t.Fatal("wait did not see the turn end")
	}
	var sh CLIShow
	cliDo(t, s, "GET", "/cli/threads/"+id+"?messages=1", "", &sh)
	if sh.State != "idle" || len(sh.Messages) != 1 || sh.Messages[0].Role != "agent" {
		t.Fatalf("show: %+v", sh)
	}
	if code := cliDo(t, s, "POST", "/cli/threads/"+id+"/send", `{"prompt":"again"}`, nil); code != 200 {
		t.Fatalf("send: %d", code)
	}
}
