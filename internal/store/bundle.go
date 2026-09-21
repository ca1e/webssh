package store

// webssh-bundle-v1 主机配置导入/导出格式:
//
//	{"format":"webssh-bundle-v1",
//	 "kdf":{"algo":"scrypt","salt":"<b64 16B>","N":32768,"r":8,"p":1} | null,
//	 "hosts":[{"wire":"<b64>","nonce":"<b64 12B>","aad":"name|host"}]  // 加密
//	 | [ {HostWire 对象,凭据 base64 字段} ]                            // 明文}
//
// 加密模式:密钥 = scrypt(导出口令, salt),与 master.key 无关——master.key
// 永不出本机;每主机独立 AES-GCM,nonce 12B/条,aad 绑定 name|host 防条目
// 调换,口令错/篡改在 GCM Open 处统一失败(AEAD 自带完整性)。
// 两种模式的 wire 载荷同构:都是 HostWire 对象、凭据以 base64 内嵌
// (bundle 专用载荷;hosts.json 磁盘格式只存引用),差别只在是否再包一层
// 加密信封。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// BundleFormat bundle 顶层格式标识;缺失或不符即拒绝。
const BundleFormat = "webssh-bundle-v1"

// bundle 加密 KDF 参数;salt 16B 随机,N/r/p 取 OWASP 推荐档位。
const (
	bundleSaltBytes = 16
	bundleKeyBytes  = 32
	bundleN         = 32768
	bundleR         = 8
	bundleP         = 1
	nonceBytes      = 12
)

// ErrBundleFormat 顶层不是 webssh-bundle-v1 对象或 format 不符。
var ErrBundleFormat = errors.New("unsupported bundle format")

// ErrDecrypt 解密失败:口令错误或文件被篡改/损坏。
var ErrDecrypt = errors.New("bundle decrypt failed")

type bundleKDF struct {
	Algo string `json:"algo"`
	Salt []byte `json:"salt"`
	N    int    `json:"N"`
	R    int    `json:"r"`
	P    int    `json:"p"`
}

type bundleEntry struct {
	Wire  string `json:"wire"`
	Nonce string `json:"nonce,omitempty"`
	AAD   string `json:"aad,omitempty"`
}

type bundleDoc struct {
	Format string        `json:"format"`
	KDF    *bundleKDF    `json:"kdf"`
	Hosts  []bundleEntry `json:"hosts"`
}

// EncodeBundleEncrypted 加密导出:逐条 AES-GCM 封装 wire。
func EncodeBundleEncrypted(wires []HostWire, password string) ([]byte, error) {
	if password == "" {
		return nil, errors.New("加密导出需要口令")
	}
	salt := make([]byte, bundleSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, err := scrypt.Key([]byte(password), salt, bundleN, bundleR, bundleP, bundleKeyBytes)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	doc := bundleDoc{
		Format: BundleFormat,
		KDF:    &bundleKDF{Algo: "scrypt", Salt: salt, N: bundleN, R: bundleR, P: bundleP},
		Hosts:  make([]bundleEntry, 0, len(wires)),
	}
	for _, w := range wires {
		plain, err := json.Marshal(w)
		if err != nil {
			return nil, err
		}
		nonce := make([]byte, nonceBytes)
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
		aad := w.Name + "|" + w.Host
		doc.Hosts = append(doc.Hosts, bundleEntry{
			Wire:  base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, []byte(aad))),
			Nonce: base64.StdEncoding.EncodeToString(nonce),
			AAD:   aad,
		})
	}
	return marshalBundle(doc)
}

// EncodeBundlePlaintext 无密码导出:kdf=null,hosts 直接存 HostWire 对象
// (凭据 base64)。仅在用户显式点击"无密码导出"时由 handler 调用。
func EncodeBundlePlaintext(wires []HostWire) ([]byte, error) {
	doc := bundleDoc{Format: BundleFormat, Hosts: make([]bundleEntry, 0, len(wires))}
	for _, w := range wires {
		plain, err := json.Marshal(w)
		if err != nil {
			return nil, err
		}
		doc.Hosts = append(doc.Hosts, bundleEntry{Wire: base64.StdEncoding.EncodeToString(plain)})
	}
	return marshalBundle(doc)
}

func marshalBundle(doc bundleDoc) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeBundle 解析 bundle:kdf 非 null 用口令解密,null 直通;两路产出同一
// wire 形态(HostWire,凭据 base64),后续 ImportWire 统一换引用。
// 顶层必须是对象且 format 匹配,否则返回 ErrBundleFormat。
func DecodeBundle(data []byte, password string) ([]HostWire, error) {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] == '[' {
		return nil, ErrBundleFormat
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc bundleDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("bundle 结构不合法: %w", err)
	}
	if doc.Format != BundleFormat {
		return nil, ErrBundleFormat
	}

	switch {
	case doc.KDF != nil:
		kdf := doc.KDF
		if kdf.Algo != "scrypt" {
			return nil, fmt.Errorf("不支持的 KDF 算法 %q", kdf.Algo)
		}
		if password == "" {
			return nil, errors.New("该 bundle 已加密,导入需要口令")
		}
		if len(kdf.Salt) != bundleSaltBytes || kdf.N <= 0 || kdf.R <= 0 || kdf.P <= 0 {
			return nil, errors.New("KDF 参数不合法")
		}
		key, err := scrypt.Key([]byte(password), kdf.Salt, kdf.N, kdf.R, kdf.P, bundleKeyBytes)
		if err != nil {
			return nil, err
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		return decodeEntries(doc.Hosts, func(e bundleEntry) ([]byte, error) {
			nonce, err := base64.StdEncoding.DecodeString(e.Nonce)
			if err != nil || len(nonce) != nonceBytes {
				return nil, ErrDecrypt
			}
			ct, err := base64.StdEncoding.DecodeString(e.Wire)
			if err != nil {
				return nil, ErrDecrypt
			}
			// GCM Open 同时校验 aad:口令错/密文篡改/aad 调换统一在此失败,
			// 不区分细节避免向调用方泄露可探测信息。
			plain, err := gcm.Open(nil, nonce, ct, []byte(e.AAD))
			if err != nil {
				return nil, ErrDecrypt
			}
			// aad 字段本身可被改写,解密后与 wire 内容二次核对防条目调换。
			var probe struct {
				Name string `json:"name"`
				Host string `json:"host"`
			}
			if err := json.Unmarshal(plain, &probe); err != nil {
				return nil, ErrDecrypt
			}
			if subtle.ConstantTimeCompare([]byte(probe.Name+"|"+probe.Host), []byte(e.AAD)) != 1 {
				return nil, ErrDecrypt
			}
			return plain, nil
		})
	default:
		return decodeEntries(doc.Hosts, func(e bundleEntry) ([]byte, error) {
			return base64.StdEncoding.DecodeString(e.Wire)
		})
	}
}

// decodeEntries 逐条解码 wire 为 HostWire;单条 JSON 不合法返回错误
// (加密路 = 完整性破坏,明文路 = 文件损坏),由调用方决定报错粒度。
func decodeEntries(entries []bundleEntry, open func(bundleEntry) ([]byte, error)) ([]HostWire, error) {
	wires := make([]HostWire, 0, len(entries))
	for i, e := range entries {
		plain, err := open(e)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条主机记录解码失败: %w", i+1, err)
		}
		dec := json.NewDecoder(bytes.NewReader(plain))
		dec.DisallowUnknownFields()
		var w HostWire
		if err := dec.Decode(&w); err != nil {
			return nil, fmt.Errorf("第 %d 条主机记录结构不合法: %w", i+1, err)
		}
		wires = append(wires, w)
	}
	return wires, nil
}

// FriendlyBundleError 把 bundle 错误翻译为用户文案(导入/导出共用)。
func FriendlyBundleError(err error) string {
	switch {
	case errors.Is(err, ErrBundleFormat):
		return "不是有效的 WebSSH 主机 bundle 文件(请使用 WebSSH 导出功能生成的文件)"
	case errors.Is(err, ErrDecrypt):
		return "口令错误或文件已损坏"
	default:
		if strings.Contains(err.Error(), "需要口令") {
			return "该 bundle 已加密,请在口令框填写导出时设置的口令"
		}
		return err.Error()
	}
}
