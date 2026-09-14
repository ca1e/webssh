package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testManager() *Manager {
	return NewManager(func(sid string) (Side, string, error) {
		return nil, "", fmt.Errorf("集成测试外的会话解析不可用")
	})
}

func writeTree(t *testing.T) (srcRoot, dstRoot string) {
	t.Helper()
	srcRoot = t.TempDir()
	dstRoot = t.TempDir()
	mustWrite := func(p, content string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(srcRoot, "a.txt"), "12345")
	mustWrite(filepath.Join(srcRoot, "sub", "b.txt"), "abc")
	mustWrite(filepath.Join(srcRoot, "sub", "deep", "c.txt"), "xy")
	return srcRoot, dstRoot
}

func TestBuildPlan(t *testing.T) {
	srcRoot, dstRoot := writeTree(t)
	src, dst := NewLocalSide(), NewLocalSide()
	dirs, files, total, err := buildPlan(src, dst, srcRoot, filepath.Join(dstRoot, "root"))
	if err != nil {
		t.Fatalf("buildPlan 失败: %v", err)
	}
	if len(dirs) != 2 { // sub 与 sub/deep
		t.Fatalf("dirs = %+v, want 2 项", dirs)
	}
	if len(files) != 3 {
		t.Fatalf("files = %+v, want 3 项", files)
	}
	if total != int64(len("12345")+len("abc")+len("xy")) {
		t.Fatalf("total = %d", total)
	}
	// 目标路径必须挂在目标根下(按目标端点约定拼接)
	dstPrefix := filepath.Join(dstRoot, "root")
	for _, f := range files {
		if !strings.HasPrefix(f.dst, dstPrefix) {
			t.Fatalf("目标路径越界: %s", f.dst)
		}
	}
}

func TestManagerLocalDirTransfer(t *testing.T) {
	srcRoot, dstRoot := writeTree(t)
	m := testManager()
	task, err := m.Create("t1",
		Endpoint{Kind: "local", Path: srcRoot},
		Endpoint{Kind: "local", Path: filepath.Join(dstRoot, "root")},
		true)
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if task.Status != StatusRunning {
		t.Fatalf("新任务状态 = %s", task.Status)
	}
	if !m.WaitIdle(5 * time.Second) {
		t.Fatal("任务未在限期内完成")
	}
	got, ok := m.Get("t1")
	if !ok {
		t.Fatal("任务记录丢失")
	}
	if got.Status != StatusDone || got.FilesDone != 3 || got.Done != 10 {
		t.Fatalf("任务结果 = %+v", got)
	}
	// 目标树内容一致
	for _, rel := range []string{"a.txt", "sub/b.txt", filepath.Join("sub", "deep", "c.txt")} {
		want, err := os.ReadFile(filepath.Join(srcRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		gotBytes, err := os.ReadFile(filepath.Join(dstRoot, "root", rel))
		if err != nil {
			t.Fatalf("目标缺失 %s: %v", rel, err)
		}
		if string(want) != string(gotBytes) {
			t.Fatalf("内容不一致: %s", rel)
		}
	}
}

func TestManagerCreateValidation(t *testing.T) {
	srcRoot, dstRoot := writeTree(t)
	m := testManager()

	// 目录未开递归
	if _, err := m.Create("t1",
		Endpoint{Kind: "local", Path: srcRoot},
		Endpoint{Kind: "local", Path: dstRoot}, false); err == nil {
		t.Fatal("目录传输未开递归应报错")
	}
	// 源路径不存在
	if _, err := m.Create("t2",
		Endpoint{Kind: "local", Path: filepath.Join(srcRoot, "nope")},
		Endpoint{Kind: "local", Path: dstRoot}, false); err == nil {
		t.Fatal("源路径不存在应报错")
	}
	// 非法 kind
	if _, err := m.Create("t3",
		Endpoint{Kind: "bad", Path: srcRoot},
		Endpoint{Kind: "local", Path: dstRoot}, false); err == nil {
		t.Fatal("非法 kind 应报错")
	}
	// 会话端点缺 sid
	if _, err := m.Create("t4",
		Endpoint{Kind: "session", Path: "/tmp"},
		Endpoint{Kind: "local", Path: dstRoot}, false); err == nil {
		t.Fatal("会话端点缺 sid 应报错")
	}
	// 会话解析失败
	if _, err := m.Create("t5",
		Endpoint{Kind: "session", SID: "ghost", Path: "/tmp"},
		Endpoint{Kind: "local", Path: dstRoot}, false); err == nil {
		t.Fatal("会话不存在应报错")
	}
}

func TestManagerCancelAndRemove(t *testing.T) {
	srcRoot, dstRoot := writeTree(t)
	m := testManager()
	if _, err := m.Create("t1",
		Endpoint{Kind: "local", Path: srcRoot},
		Endpoint{Kind: "local", Path: filepath.Join(dstRoot, "root")}, true); err != nil {
		t.Fatal(err)
	}
	// 运行中或已完成均可取消/移除;移除后记录消失
	m.Cancel("t1")
	if !m.Remove("t1") {
		t.Fatal("Remove 应成功")
	}
	if _, ok := m.Get("t1"); ok {
		t.Fatal("移除后记录应消失")
	}
	if !m.WaitIdle(5 * time.Second) {
		t.Fatal("任务未收尾")
	}
}
