package handler

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"webssh/internal/k8smgr"
)

// ---------- k8sNsSelector:namespace 参数 → kubectl 范围参数 ----------

func TestK8sNsSelector(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"", "-A", false},
		{"_all", "-A", false},
		{"kube-system", "-n 'kube-system'", false},
		{"my.ns-1", "-n 'my.ns-1'", false},
		{"../etc", "", true}, // 路径穿越
		{"Upper", "", true},  // 大写不合法
		{"a b", "", true},    // 空白
		{"a'b", "", true},    // 引号注入
	}
	for _, tc := range cases {
		got, err := k8sNsSelector(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("%q: 期望报错,实际返回 %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: 不应报错,实际 %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------- k8sDocStream:watch 流的文档拆解与终止语义 ----------

// collectDocs 包装 emit,收集全部输出文档。
func collectDocs(r io.Reader) (docs []json.RawMessage) {
	k8sDocStream(r, func(doc json.RawMessage) bool {
		docs = append(docs, doc)
		return true
	})
	return
}

func docString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	return strings.TrimSpace(string(raw))
}

func TestK8sDocStreamListThenEvents(t *testing.T) {
	stream := `{"kind":"List","items":[{"metadata":{"name":"p1"}}]}
{"type":"ADDED","object":{"metadata":{"name":"p2"}}}
{"type":"MODIFIED","object":{"metadata":{"name":"p1"}}}
{"type":"DELETED","object":{"metadata":{"name":"p2"}}}
`
	docs := collectDocs(strings.NewReader(stream))
	if len(docs) != 5 {
		t.Fatalf("文档数 = %d, want 5(4 文档 + 1 终止帧): %s", len(docs), docs)
	}
	if !strings.Contains(docString(t, docs[0]), `"kind":"List"`) {
		t.Errorf("首文档应为 List 快照: %s", docs[0])
	}
	for i, typ := range []string{"ADDED", "MODIFIED", "DELETED", "exit"} {
		var m map[string]any
		if err := json.Unmarshal(docs[i+1], &m); err != nil || m["type"] != typ {
			t.Errorf("文档 %d: type = %v, want %q", i+1, m["type"], typ)
		}
	}
}

func TestK8sDocStreamExitDocStopsReading(t *testing.T) {
	stream := `{"kind":"List","items":[]}
{"type":"exit","error":"kubectl: forbidden"}
{"type":"ADDED","object":{}}
`
	docs := collectDocs(strings.NewReader(stream))
	if len(docs) != 2 {
		t.Fatalf("exit 文档后应停止,文档数 = %d: %s", len(docs), docs)
	}
	if !strings.Contains(docString(t, docs[1]), "forbidden") {
		t.Errorf("终止帧应透传错误详情: %s", docs[1])
	}
}

func TestK8sDocStreamGarbageYieldsErrorExit(t *testing.T) {
	docs := collectDocs(strings.NewReader("this is not json\n"))
	if len(docs) != 1 {
		t.Fatalf("坏数据应只产出终止帧,文档数 = %d: %s", len(docs), docs)
	}
	var m map[string]any
	if err := json.Unmarshal(docs[0], &m); err != nil || m["type"] != "exit" || m["error"] == "" {
		t.Errorf("终止帧应含 error: %s", docs[0])
	}
}

func TestK8sDocStreamEmptyInput(t *testing.T) {
	docs := collectDocs(strings.NewReader(""))
	if len(docs) != 1 {
		t.Fatalf("空流应产出一条终止帧,文档数 = %d", len(docs))
	}
	var m map[string]any
	if err := json.Unmarshal(docs[0], &m); err != nil || m["error"] != nil {
		t.Errorf("正常 EOF 的终止帧不应带 error: %s", docs[0])
	}
}

// ---------- 远端脚本构造 ----------

func TestK8sExecScriptFallback(t *testing.T) {
	script := k8smgr.ExecScript("ns-1", "pod-1", "")
	if !strings.Contains(script, "exec -it -n 'ns-1' 'pod-1' -- bash") ||
		!strings.Contains(script, "-- sh") {
		t.Errorf("应含 bash→sh 回退: %s", script)
	}
	withC := k8smgr.ExecScript("ns-1", "pod-1", "c-1")
	if !strings.Contains(withC, "-c 'c-1'") {
		t.Errorf("应含容器参数: %s", withC)
	}
}

func TestK8sStreamScriptErrorFolding(t *testing.T) {
	script := k8smgr.StreamScript("get pods -A -w --output-watch-events -o json")
	for _, want := range []string{"command -v kubectl", `--output-watch-events`, `{"type":"exit","error":"kubectl: `} {
		if !strings.Contains(script, want) {
			t.Errorf("脚本缺少 %q", want)
		}
	}
}
