package sshmgr

import "testing"

func TestResolveListenPort(t *testing.T) {
	if err := resolveListenPort("k8s", 0); err != nil {
		t.Errorf("k8s 应允许 0(自动分配): %v", err)
	}
	for _, typ := range []string{"local", "socks5"} {
		if err := resolveListenPort(typ, 0); err == nil {
			t.Errorf("%s 不应允许 0", typ)
		}
	}
	if err := resolveListenPort("k8s", 18080); err != nil {
		t.Errorf("显式合法端口应通过: %v", err)
	}
	if err := resolveListenPort("k8s", 70000); err == nil {
		t.Error("越界端口应被拒")
	}
}

// ListForwards 快照应携带原子计数器的当前值(OpenLens 式面板数据源)。
func TestListForwardsStatsSnapshot(t *testing.T) {
	s := &Session{forwards: map[string]*Forward{}}
	f := &Forward{ID: "f1", Type: "k8s", ListenPort: 18080, sess: s}
	f.totalN.Add(5)
	f.activeN.Add(2)
	f.recvN.Add(1024)
	f.sentN.Add(256)
	s.forwards["f1"] = f

	list := s.ListForwards()
	if len(list) != 1 {
		t.Fatalf("应有 1 条转发,got %d", len(list))
	}
	st := list[0].Stats
	if st.Active != 2 || st.Total != 5 || st.Recv != 1024 || st.Sent != 256 {
		t.Fatalf("统计快照不正确: %+v", st)
	}
}
