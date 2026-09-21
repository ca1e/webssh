package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"webssh/internal/secrets"
)

// bundleFixture 造一个含凭据主机的 bundle 测试环境。
func bundleFixture(t *testing.T) (dir string, wires []HostWire, sec *secrets.Store) {
	t.Helper()
	dir = t.TempDir()
	sec, err := secrets.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := sec.Put("", secrets.KindPassword, "bundle-pw")
	pp, _ := sec.Put("", secrets.KindPassphrase, "bundle-pp")
	wires = []HostWire{{
		hostWire: hostWire{
			ID: "b1", Name: "node-a", Host: "10.1.1.1", Port: 22,
			Username: "root", AuthType: AuthPassword,
		},
		Password: base64.StdEncoding.EncodeToString([]byte("bundle-pw")),
	}, {
		hostWire: hostWire{
			ID: "b2", Name: "node-b", Host: "10.1.1.2", Port: 2222,
			Username: "deploy", AuthType: AuthKey, KeyPath: "~/.ssh/id_ed25519",
			PasswordRef: ref, PassphraseRef: pp,
		},
		Passphrase: base64.StdEncoding.EncodeToString([]byte("bundle-pp")),
	}}
	return dir, wires, sec
}

// TestBundleEncryptedRoundTrip 加密导出 → 正确口令导入:wire 完整还原。
func TestBundleEncryptedRoundTrip(t *testing.T) {
	_, wires, _ := bundleFixture(t)
	data, err := EncodeBundleEncrypted(wires, "pass-1234")
	if err != nil {
		t.Fatal(err)
	}
	// 顶层结构与文档 §3.3 一致
	var doc struct {
		Format string `json:"format"`
		KDF    *struct {
			Algo string `json:"algo"`
			N    int    `json:"N"`
		} `json:"kdf"`
		Hosts []map[string]any `json:"hosts"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Format != BundleFormat || doc.KDF == nil || doc.KDF.Algo != "scrypt" || doc.KDF.N != bundleN {
		t.Fatalf("bundle 顶层结构不符: format=%q kdf=%+v", doc.Format, doc.KDF)
	}
	if len(doc.Hosts) != 2 {
		t.Fatalf("hosts 条数 = %d", len(doc.Hosts))
	}
	// 密文不得含 base64 明文凭据
	if strings.Contains(string(data), base64.StdEncoding.EncodeToString([]byte("bundle-pw"))) {
		t.Fatal("加密 bundle 泄露凭据")
	}
	got, err := DecodeBundle(data, "pass-1234")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "node-a" || got[0].Password == "" {
		t.Fatalf("解密结果不符: %+v", got)
	}
	// 明文 b64 凭据还原正确
	if pt, _ := base64.StdEncoding.DecodeString(got[0].Password); string(pt) != "bundle-pw" {
		t.Fatalf("凭据不符: %q", pt)
	}
}

func TestBundlePlaintextRoundTrip(t *testing.T) {
	_, wires, _ := bundleFixture(t)
	data, err := EncodeBundlePlaintext(wires)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		KDF   *bundleKDF `json:"kdf"`
		Hosts []bundleEntry
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.KDF != nil {
		t.Fatal("明文模式 kdf 应为 null")
	}
	got, err := DecodeBundle(data, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Name != "node-b" {
		t.Fatalf("明文直通解码不符: %+v", got)
	}
	if pt, _ := base64.StdEncoding.DecodeString(got[1].Passphrase); string(pt) != "bundle-pp" {
		t.Fatalf("口令不符: %q", pt)
	}
}

// TestBundleEncryptedRejections 口令错 / 密文篡改 / aad 条目调换三路拒绝。
func TestBundleEncryptedRejections(t *testing.T) {
	_, wires, _ := bundleFixture(t)
	data, err := EncodeBundleEncrypted(wires, "right-pass")
	if err != nil {
		t.Fatal(err)
	}

	// 1) 口令错误
	if _, err := DecodeBundle(data, "wrong-pass"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("错误口令应 ErrDecrypt,得到 %v", err)
	}

	// 2) 密文篡改:翻转第一条 wire 的一个字符
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	hosts := doc["hosts"].([]any)
	first := hosts[0].(map[string]any)
	wire := first["wire"].(string)
	raw, _ := base64.StdEncoding.DecodeString(wire)
	raw[3] ^= 0x01
	first["wire"] = base64.StdEncoding.EncodeToString(raw)
	tampered, _ := json.Marshal(doc)
	if _, err := DecodeBundle(tampered, "right-pass"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("密文篡改应 ErrDecrypt,得到 %v", err)
	}

	// 3) aad 条目调换:交换两条的 (wire,nonce,aad) 整体条目顺序
	//    (整体调换语义上等价于改名,但内容真实完整;真实攻击是把 A 条目的
	//    密文塞进 B 条目而 aad 保持 B 的,此处模拟后者)
	hosts[0].(map[string]any)["aad"] = "evil|host"
	swapped, _ := json.Marshal(doc)
	if _, err := DecodeBundle(swapped, "right-pass"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("aad 与密文不符应 ErrDecrypt,得到 %v", err)
	}
}

func TestBundleInvalidFormatRejected(t *testing.T) {
	data := []byte(`[{"id":"a","name":"old","host":"h","username":"u","auth_type":"password","password":"abc"}]`)
	_, err := DecodeBundle(data, "")
	if !errors.Is(err, ErrBundleFormat) {
		t.Fatalf("数组格式应 ErrBundleFormat,得到 %v", err)
	}
	if msg := FriendlyBundleError(err); !strings.Contains(msg, "bundle") {
		t.Fatalf("文案未说明格式要求: %q", msg)
	}
	// 未知 format 同样拒绝
	_, err = DecodeBundle([]byte(`{"format":"other","kdf":null,"hosts":[]}`), "")
	if !errors.Is(err, ErrBundleFormat) {
		t.Fatalf("未知 format 应 ErrBundleFormat,得到 %v", err)
	}
}

func TestBundleUnknownFieldsRejected(t *testing.T) {
	// 明文 wire 内的未知字段必须被 DisallowUnknownFields 拒绝
	doc := bundleDoc{Format: BundleFormat, Hosts: []bundleEntry{{
		Wire: base64.StdEncoding.EncodeToString([]byte(`{"id":"a","evil":true}`)),
	}}}
	data, err := marshalBundle(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBundle(data, ""); err == nil {
		t.Fatal("wire 未知字段应被拒绝")
	}
}

func TestBundleEncryptedNeedsPassword(t *testing.T) {
	_, wires, _ := bundleFixture(t)
	data, err := EncodeBundleEncrypted(wires, "pass-1234")
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeBundle(data, "")
	if err == nil || errors.Is(err, ErrDecrypt) {
		t.Fatalf("缺口令应明确报错,得到 %v", err)
	}
	if msg := FriendlyBundleError(err); !strings.Contains(msg, "口令") {
		t.Fatalf("缺口令文案不符: %q", msg)
	}
}
