package a2ashape

// UX-T13 (ux §6.1, core G13): the language comes from the environment, and
// UX-T1 holds in both: once with LC_ALL=zh_CN.UTF-8, once with LANG=C.

import (
	"testing"
)

func TestLangFromTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want Language
	}{
		{map[string]string{}, English},
		{map[string]string{"LANG": "C"}, English},
		{map[string]string{"LANG": "zh_CN.UTF-8"}, Chinese},
		{map[string]string{"LANG": "zh_TW.UTF-8"}, Chinese},
		{map[string]string{"LANG": "en_US.UTF-8"}, English},
		// The first non-empty of LC_ALL, LC_MESSAGES, LANG decides.
		{map[string]string{"LC_ALL": "en_US.UTF-8", "LANG": "zh_CN.UTF-8"}, English},
		{map[string]string{"LC_ALL": "zh_CN.UTF-8", "LANG": "C"}, Chinese},
		{map[string]string{"LC_ALL": "", "LC_MESSAGES": "zh_CN.UTF-8", "LANG": "C"}, Chinese},
		{map[string]string{"LC_MESSAGES": "C", "LANG": "zh_CN.UTF-8"}, English},
		// "zh" begins the value, or it is not Chinese.
		{map[string]string{"LANG": "en_ZH"}, English},
		{map[string]string{"LANG": "ZH_CN"}, English},
	} {
		got := LangFrom(func(k string) string { return c.env[k] })
		if got != c.want {
			t.Errorf("%v: %d, want %d", c.env, got, c.want)
		}
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
		{"LC_ALL=zh_CN.UTF-8", map[string]string{"LC_ALL": "zh_CN.UTF-8", "LC_MESSAGES": "", "LANG": ""}, Chinese},
		{"LANG=C", map[string]string{"LC_ALL": "", "LC_MESSAGES": "", "LANG": "C"}, English},
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
	t.Setenv("LC_ALL", "zh_CN.UTF-8")
	zh := Sentence("denied", Lang(), nil)
	t.Setenv("LC_ALL", "C")
	if en := Sentence("denied", Lang(), nil); en == zh {
		t.Fatal("the language did not change with the environment")
	}
}
