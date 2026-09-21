package secrets

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openStore(t *testing.T) (string, *Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return dir, s
}

func TestPutRevealRoundTrip(t *testing.T) {
	_, s := openStore(t)
	ref, err := s.Put("", KindPassword, "s3cret|pw")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Reveal(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cret|pw" {
		t.Fatalf("Reveal = %q", got)
	}
	// 同 id 原位替换
	ref2, err := s.Put(ref, KindPassphrase, "new")
	if err != nil {
		t.Fatal(err)
	}
	if ref2 != ref {
		t.Fatalf("Put(id) 应原位替换,得到 %q != %q", ref2, ref)
	}
	if v, _ := s.Reveal(ref); v != "new" {
		t.Fatalf("替换后 Reveal = %q", v)
	}
	// 删除后不可解
	if err := s.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reveal(ref); err == nil {
		t.Fatal("Delete 后 Reveal 应报错")
	}
}

func TestPersistenceAcrossOpen(t *testing.T) {
	dir, s := openStore(t)
	ref, err := s.Put("", KindPassword, "persist-me")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s2.Reveal(ref); err != nil || v != "persist-me" {
		t.Fatalf("重开后 Reveal = %q, %v", v, err)
	}
	// master.key 权限须为 0600
	fi, err := os.Stat(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("master.key 权限 = %o, want 600", fi.Mode().Perm())
	}
	sfi, err := os.Stat(filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sfi.Mode().Perm() != 0o600 {
		t.Fatalf("secrets.json 权限 = %o, want 600", sfi.Mode().Perm())
	}
	// 明文不得出现在磁盘
	raw, _ := os.ReadFile(filepath.Join(dir, "secrets.json"))
	if strings.Contains(string(raw), "persist-me") {
		t.Fatal("secrets.json 含明文凭据")
	}
}

func TestTamperRejected(t *testing.T) {
	dir, s := openStore(t)
	ref, err := s.Put("", KindPassword, "integrity")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secrets.json")
	raw, _ := os.ReadFile(path)
	// 翻转密文一个字符(位于 base64 体内)
	idx := strings.Index(string(raw), `"ciphertext": "`)
	if idx < 0 {
		t.Fatal("未找到 ciphertext 字段")
	}
	pos := idx + len(`"ciphertext": "`) + 5
	tampered := []byte(raw)
	if tampered[pos] == 'A' {
		tampered[pos] = 'B'
	} else {
		tampered[pos] = 'A'
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Reveal(ref); err == nil {
		t.Fatal("篡改密文后 Reveal 应报错")
	}
}

func TestCrossRecordSwapRejected(t *testing.T) {
	dir, s := openStore(t)
	a, _ := s.Put("", KindPassword, "AAA")
	b, _ := s.Put("", KindPassword, "BBB")
	// 把 a 的密文搬到 b 名下(记录调换):AAD 绑 id,GCM Open 必失败
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	recA := s2.records[a]
	recA.ID = b
	s2.records[b] = recA
	if _, err := s2.Reveal(b); err == nil {
		t.Fatal("记录调换后解密应因 AAD 失败")
	}
}

func TestKeyMissingExplicitError(t *testing.T) {
	dir, s := openStore(t)
	if _, err := s.Put("", KindPassword, "x"); err != nil {
		t.Fatal(err)
	}
	// 删 key 留 secrets.json → 显式报错,不静默重建
	if err := os.Remove(filepath.Join(dir, "master.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("key 缺失时应报错")
	} else if !strings.Contains(err.Error(), "master.key") {
		t.Fatalf("错误应指向 master.key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "master.key")); err == nil {
		t.Fatal("不得静默重建 master.key")
	}
}

func TestEnvKeyOverride(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	t.Setenv("WEBSSH_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Put("", KindPassword, "envkey")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Reveal(ref); v != "envkey" {
		t.Fatalf("Reveal = %q", v)
	}
	// 环境变量模式下不得生成 master.key 文件
	if _, err := os.Stat(filepath.Join(dir, "master.key")); !os.IsNotExist(err) {
		t.Fatal("WEBSSH_MASTER_KEY 模式下不应写 master.key")
	}
	// 非法 base64 报错
	t.Setenv("WEBSSH_MASTER_KEY", "!!!not-base64!!!")
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("非法 WEBSSH_MASTER_KEY 应报错")
	}
}

func TestConcurrentPut(t *testing.T) {
	_, s := openStore(t)
	var wg sync.WaitGroup
	refs := make([]string, 32)
	for i := range refs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref, err := s.Put("", KindPassword, strings.Repeat("x", i)+string(rune('a'+i%26)))
			if err != nil {
				t.Errorf("Put %d: %v", i, err)
				return
			}
			refs[i] = ref
		}(i)
	}
	wg.Wait()
	for i, ref := range refs {
		if ref == "" {
			continue
		}
		v, err := s.Reveal(ref)
		if err != nil || v != strings.Repeat("x", i)+string(rune('a'+i%26)) {
			t.Fatalf("并发 Put 后 Reveal(%d) = %q, %v", i, v, err)
		}
	}
}
