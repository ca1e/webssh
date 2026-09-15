package store

import (
	"encoding/json"
	"testing"
)

func TestHostWireRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// telnet 主机:conn_type 需持久化并在重开后保留
	h, err := s.Put(Host{Name: "console", Host: "10.0.0.1", ConnType: ConnTelnet})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get(h.ID)
	if !ok {
		t.Fatal("重开存储后主机丢失")
	}
	if got.ConnType != ConnTelnet {
		t.Fatalf("ConnType = %q, want %q", got.ConnType, ConnTelnet)
	}

	// SSH 主机:FollowSSHPath 与密码需随 wire 持久化(FollowSSHPath 此前缺失,重启即丢)
	h2, err := s2.Put(Host{
		Name: "dev", Host: "10.0.0.2", Username: "root",
		AuthType: AuthPassword, Password: "pw", FollowSSHPath: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	s3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got2, ok := s3.Get(h2.ID)
	if !ok {
		t.Fatal("重开存储后主机丢失")
	}
	if got2.ConnType != ConnSSH {
		t.Fatalf("未填 conn_type 的主机应回落 %q,得到 %q", ConnSSH, got2.ConnType)
	}
	if !got2.FollowSSHPath {
		t.Fatal("FollowSSHPath 未持久化")
	}
	if got2.Password != "pw" {
		t.Fatalf("Password = %q, want pw", got2.Password)
	}
}

// TestFromWireLegacyRecord 旧版本 hosts.json 记录(无 conn_type/follow_ssh_path
// 字段)须能正常加载并回落为 SSH。
func TestFromWireLegacyRecord(t *testing.T) {
	var w hostWire
	raw := `{"id":"a","name":"legacy","host":"h","port":22,"username":"u","auth_type":"password"}`
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	h, err := fromWire(w)
	if err != nil {
		t.Fatal(err)
	}
	if h.ConnType != ConnSSH {
		t.Fatalf("旧记录 ConnType = %q, want %q", h.ConnType, ConnSSH)
	}
	if h.FollowSSHPath {
		t.Fatal("旧记录 FollowSSHPath 应为 false")
	}
}

func TestNormalizeConnType(t *testing.T) {
	for in, want := range map[string]string{
		"":         ConnSSH,
		"ssh":      ConnSSH,
		"telnet":   ConnTelnet,
		"TELNET":   ConnSSH,
		"whatever": ConnSSH,
	} {
		if got := NormalizeConnType(in); got != want {
			t.Errorf("NormalizeConnType(%q) = %q, want %q", in, got, want)
		}
	}
}
