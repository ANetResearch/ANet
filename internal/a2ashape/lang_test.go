package a2ashape

// UX-T13 (ux §6.1): the language comes from ANET_LANG alone, and UX-T1 holds
// in both: once with ANET_LANG=zh-CN, once with it unset.

import (
	"strings"
	"testing"
)

func TestLangFromTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want Language
	}{
		{map[string]string{}, English},
		{map[string]string{"ANET_LANG": "zh-CN"}, Chinese},
		{map[string]string{"ANET_LANG": "zh"}, Chinese},
		{map[string]string{"ANET_LANG": "zh_CN.UTF-8"}, Chinese},
		{map[string]string{"ANET_LANG": "ZH"}, Chinese},
		{map[string]string{"ANET_LANG": "en"}, English},
		{map[string]string{"ANET_LANG": "C"}, English},
		{map[string]string{"ANET_LANG": "en_ZH"}, English},
		// The locale variables are not consulted.
		{map[string]string{"LANG": "zh_CN.UTF-8"}, English},
		{map[string]string{"LC_ALL": "zh_CN.UTF-8"}, English},
		{map[string]string{"LC_MESSAGES": "zh_CN.UTF-8"}, English},
		{map[string]string{"ANET_LANG": "en", "LC_ALL": "zh_CN.UTF-8", "LANG": "zh_CN.UTF-8"}, English},
		{map[string]string{"ANET_LANG": "zh", "LC_ALL": "C", "LANG": "C"}, Chinese},
	} {
		got := LangFrom(func(k string) string { return c.env[k] })
		if got != c.want {
			t.Errorf("%v: %d, want %d", c.env, got, c.want)
		}
	}
}

// The rows of ClassModel are read by the agent in the sandbox: English in
// both languages, the next step included.
func TestModelRowsAreEnglishInBothLanguages(t *testing.T) {
	n := 0
	for _, r := range Rows() {
		if r.Class&ClassModel == 0 {
			continue
		}
		n++
		args := Args{"host": "example.org", "port": 25, "n": 1, "limit": 1, "code": "x"}
		zh, en := Sentence(r.Key(), Chinese, args), Sentence(r.Key(), English, args)
		if zh != en {
			t.Errorf("%s: Chinese render %q differs from English %q", r.Key(), zh, en)
		}
		if strings.ContainsFunc(zh, func(c rune) bool { return c >= 0x4e00 && c <= 0x9fff }) {
			t.Errorf("%s: model-facing sentence has Chinese: %q", r.Key(), zh)
		}
	}
	if n == 0 {
		t.Fatal("no ClassModel rows")
	}
}

// UX-T13: UX-T1 under each language, read from the process environment as
// the daemon reads it.
func TestContractInBothLanguages(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		want Language
	}{
		{"ANET_LANG=zh-CN", map[string]string{"ANET_LANG": "zh-CN", "LC_ALL": "", "LANG": ""}, Chinese},
		{"LANG=zh_CN.UTF-8 alone", map[string]string{"ANET_LANG": "", "LC_ALL": "", "LANG": "zh_CN.UTF-8"}, English},
		{"ANET_LANG=en with LC_ALL=zh_CN", map[string]string{"ANET_LANG": "en", "LC_ALL": "zh_CN.UTF-8", "LANG": ""}, English},
	} {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if got := Lang(); got != c.want {
				t.Fatalf("Lang() = %d, want %d", got, c.want)
			}
			runContract(t)
		})
	}
	t.Setenv("ANET_LANG", "zh")
	zh := Sentence("denied", Lang(), nil)
	t.Setenv("ANET_LANG", "en")
	if en := Sentence("denied", Lang(), nil); en == zh {
		t.Fatal("the language did not change with the environment")
	}
}
