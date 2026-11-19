package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func rpc(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad output line %q", l)
		}
		res = append(res, m)
	}
	return res
}

func server() *Server {
	return &Server{Name: "t", Version: "1", Tools: []Tool{
		{Name: "echo", Description: "d", Schema: map[string]any{"type": "object"}, Run: func(a json.RawMessage) (string, error) {
			var p struct{ Text string }
			_ = json.Unmarshal(a, &p)
			return "got " + p.Text, nil
		}},
		{Name: "fail", Schema: map[string]any{"type": "object"}, Run: func(json.RawMessage) (string, error) { return "", errors.New("nope") }},
	}}
}

func TestHandshakeAndTools(t *testing.T) {
	out := rpc(t, server(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, // no response
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fail"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"missing"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"nope"}`,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
		`not json`)
	if len(out) != 8 { // 7 requests + 1 parse error; the notification gets nothing
		t.Fatalf("got %d responses: %v", len(out), out)
	}
	if v := out[0]["result"].(map[string]any)["protocolVersion"]; v != "2024-11-05" {
		t.Fatalf("version negotiation: %v", v)
	}
	if n := len(out[1]["result"].(map[string]any)["tools"].([]any)); n != 2 {
		t.Fatalf("tools: %d", n)
	}
	if out[2]["id"] != "x" || !strings.Contains(string(mustJSON(out[2])), "got hi") {
		t.Fatalf("call: %v", out[2])
	}
	if out[3]["result"].(map[string]any)["isError"] != true {
		t.Fatalf("a failing tool is an isError result: %v", out[3])
	}
	if out[4]["error"].(map[string]any)["code"].(float64) != -32602 || out[5]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("errors: %v %v", out[4], out[5])
	}
	if out[7]["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Fatalf("parse error: %v", out[7])
	}
}

func TestUnknownVersionFallsBackToLatest(t *testing.T) {
	out := rpc(t, server(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if out[0]["result"].(map[string]any)["protocolVersion"] != latest {
		t.Fatal("should answer with the newest supported version")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
