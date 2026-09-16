package handler

import (
	"reflect"
	"testing"
)

func TestParseK8sTargetsSvc(t *testing.T) {
	// 模拟 kubectl jsonpath 输出:多端口、无名端口、ExternalName(无端口)
	body := "svc\tmaxscale\t3306:;\n" +
		"svc\tkubernetes\t443:https;53:dns;\n" +
		"svc\text-name\t\n"
	got := parseK8sTargets("svc", body)
	want := []k8sTarget{
		{Name: "maxscale", Ports: []k8sPort{{Port: "3306"}}},
		{Name: "kubernetes", Ports: []k8sPort{{Port: "443", Name: "https"}, {Port: "53", Name: "dns"}}},
		{Name: "ext-name", Ports: []k8sPort{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("svc 解析错误:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestParseK8sTargetsPod(t *testing.T) {
	body := "po\tredis-0\tRunning\t6379:redis;\n" +
		"po\tbusybox\tPending\t\n" +
		"garbage-line-skip-me\n"
	got := parseK8sTargets("po", body)
	want := []k8sTarget{
		{Name: "redis-0", Status: "Running", Ports: []k8sPort{{Port: "6379", Name: "redis"}}},
		{Name: "busybox", Status: "Pending", Ports: []k8sPort{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pod 解析错误:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestParseK8sTargetsEmpty(t *testing.T) {
	if got := parseK8sTargets("svc", ""); len(got) != 0 {
		t.Fatalf("空输出应得空列表,got=%+v", got)
	}
}

func TestParseK8sNS(t *testing.T) {
	body := "NAME   STATUS   AGE\nmodule   Active   21d\nkube-system   Active   21d"
	got := parseK8sNS(body)
	want := []string{"module", "kube-system"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ns 解析错误: got=%v want=%v", got, want)
	}
}
