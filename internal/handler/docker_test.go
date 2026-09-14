package handler

import (
	"testing"
	"time"
)

func TestSanitizeDockerID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc123DEF-4.5_x", "abc123DEF-4.5_x"},
		{"a'b$(id)", "abid"},
		{"a`id`b;rm -rf /", "aidbrm-rf"},
		{"容器 id", "id"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := sanitizeDockerID(tc.in); got != tc.want {
			t.Fatalf("sanitizeDockerID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc", "'abc'"},
		{"", "''"},
		{"it's", `'it'\''s'`},
		{"a b;c", "'a b;c'"},
		{"$(id)", "'$(id)'"},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Fatalf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseDockerTime(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2024-01-15 10:30:00 +0800 CST", "2024-01-15T10:30:00+08:00"},
		{"2024-01-15 10:30:00 +0000 UTC", "2024-01-15T10:30:00Z"},
		{"2026-05-01 08:00:00 -0500 EDT", "2026-05-01T08:00:00-05:00"},
	}
	for _, tc := range cases {
		if got := parseDockerTime(tc.in); got != tc.want {
			t.Fatalf("parseDockerTime(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// 无时区:按本地时区解析,与 time.Date 独立构造的期望值比对
	want := time.Date(2024, 1, 15, 10, 30, 0, 0, time.Local).Format(time.RFC3339)
	if got := parseDockerTime("2024-01-15 10:30:00"); got != want {
		t.Fatalf("parseDockerTime 无时区解析不符: got %q, want %q", got, want)
	}

	// 非法输入原样返回
	if got := parseDockerTime("not-a-time"); got != "not-a-time" {
		t.Fatalf("parseDockerTime 非法输入应原样返回,得到 %q", got)
	}
}
