package store

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"webssh/internal/secrets"
)

// openTest 组合 secrets+store 的测试惯用法(与 main.go 装配顺序一致)。
func openTest(t *testing.T) (string, *secrets.Store, *Store) {
	t.Helper()
	dir := t.TempDir()
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir, sec)
	if err != nil {
		t.Fatal(err)
	}
	return dir, sec, st
}

func TestHostWireRoundTrip(t *testing.T) {
	dir, sec, s := openTest(t)
	// telnet 主机:conn_type 需持久化并在重开后保留
	h, err := s.Put(Host{Name: "console", Host: "10.0.0.1", ConnType: ConnTelnet})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir, sec)
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

	// SSH 主机:凭据经 secrets 库往返;FollowSSHPath 随 wire 持久化
	ref, err := s2.PutPassword("", "pw")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := s2.Put(Host{
		Name: "dev", Host: "10.0.0.2", Username: "root",
		AuthType: AuthPassword, PasswordRef: ref, FollowSSHPath: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	s3, err := Open(dir, sec)
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
	if got2.PasswordRef != ref {
		t.Fatalf("PasswordRef = %q, want %q", got2.PasswordRef, ref)
	}
	if v, err := sec.Reveal(ref); err != nil || v != "pw" {
		t.Fatalf("Reveal = %q, %v", v, err)
	}
	// hosts.json 不得含明文凭据或凭据字段("password" 不是合法磁盘字段;
	// auth_type 的值 "password" 是合法元数据,不在检查之列)
	raw, _ := os.ReadFile(filepath.Join(dir, "hosts.json"))
	if strings.Contains(string(raw), base64.StdEncoding.EncodeToString([]byte("pw"))) || strings.Contains(string(raw), `"password":`) {
		t.Fatalf("hosts.json 泄露凭据: %s", raw)
	}
}

func countSecrets(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "secrets.json"))
	if err != nil {
		return 0
	}
	var f struct {
		Secrets []json.RawMessage `json:"secrets"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return len(f.Secrets)
}

// TestFromWireOptionalFieldsMissing hosts.json 记录省略可选字段
// (conn_type/follow_ssh_path)时正常加载并回落为 SSH。
func TestFromWireOptionalFieldsMissing(t *testing.T) {
	var w hostWire
	raw := `{"id":"a","name":"x","host":"h","port":22,"username":"u","auth_type":"password"}`
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	h := hostFromWire(w)
	if h.ConnType != ConnSSH {
		t.Fatalf("ConnType = %q, want %q", h.ConnType, ConnSSH)
	}
	if h.FollowSSHPath {
		t.Fatal("FollowSSHPath 应为 false")
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

// TestUpsertManyKeepsRefs 导入命中且新条目无凭据 → 保留原有引用。
func TestUpsertManyKeepsRefs(t *testing.T) {
	_, _, s := openTest(t)
	ref, err := s.PutPassword("", "keepme")
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Put(Host{Name: "web", Host: "1.2.3.4", Username: "root", AuthType: AuthPassword, PasswordRef: ref})
	if err != nil {
		t.Fatal(err)
	}
	// 导入同名同主机但无凭据
	if _, err := s.UpsertMany([]Host{{Name: "web", Host: "1.2.3.4", Username: "admin", AuthType: AuthPassword}}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(h.ID)
	if !ok {
		t.Fatal("Upsert 应保留原 ID")
	}
	if got.PasswordRef != ref {
		t.Fatalf("PasswordRef 应保留 %q,得到 %q", ref, got.PasswordRef)
	}
	if got.Username != "admin" {
		t.Fatalf("其余字段应被覆盖,Username = %q", got.Username)
	}
}

// TestExportImportWireRoundTrip 导出 wire(base64 凭据回填)→ 导入换 ref 往返。
func TestExportImportWireRoundTrip(t *testing.T) {
	_, _, s := openTest(t)
	ref, _ := s.PutPassword("", "exp-pw")
	pph, _ := s.PutPassphrase("", "exp-pp")
	if _, err := s.Put(Host{Name: "e1", Host: "9.9.9.9", Username: "u", AuthType: AuthKey, PasswordRef: ref, PassphraseRef: pph}); err != nil {
		t.Fatal(err)
	}
	wires, err := s.ExportWires()
	if err != nil {
		t.Fatal(err)
	}
	if len(wires) != 1 || wires[0].Password == "" || wires[0].Passphrase == "" {
		t.Fatalf("导出 wire 未回填凭据: %+v", wires)
	}
	if pt, _ := base64.StdEncoding.DecodeString(wires[0].Password); string(pt) != "exp-pw" {
		t.Fatalf("导出凭据错: %q", pt)
	}
	// 新库导入
	dir2 := t.TempDir()
	sec2, err := secrets.Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir2, sec2)
	if err != nil {
		t.Fatal(err)
	}
	var hosts []Host
	for _, w := range wires {
		h, err := st2.ImportWire(w)
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, h)
	}
	if _, err := st2.UpsertMany(hosts); err != nil {
		t.Fatal(err)
	}
	list := st2.List()
	if len(list) != 1 || list[0].PasswordRef == "" {
		t.Fatalf("导入后凭据未入库: %+v", list)
	}
	if v, err := sec2.Reveal(list[0].PasswordRef); err != nil || v != "exp-pw" {
		t.Fatalf("导入凭据 = %q, %v", v, err)
	}
}
