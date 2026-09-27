package provider

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// described is a fake provider that says something about some of its
// capabilities.
type described struct {
	fake
	info map[string]SkillInfo
}

func (d *described) SkillInfo(c string) (SkillInfo, bool) {
	si, ok := d.info[c]
	return si, ok
}

// A capability whose provider says nothing still gets every field A2A
// requires (name, description, non-empty tags), derived from the id alone.
func TestSkillInfoIsDerivedFromTheIDWhenTheProviderSaysNothing(t *testing.T) {
	cases := []struct {
		id       string
		wantTags []string
	}{
		{"text.digest", []string{"text", "digest"}},
		{"net.echo", []string{"net", "echo"}},
		{"system.reboot@dahua/camera-1", []string{"system", "reboot"}},
		{"Image.Inspect.image", []string{"image", "inspect"}},
		{"single", []string{"single"}},
		{"...", []string{"..."}},
	}
	for _, c := range cases {
		si := SkillInfoOf(&fake{id: "p"}, c.id)
		if si.Name != c.id {
			t.Errorf("%s: name = %q, want the id", c.id, si.Name)
		}
		if !strings.Contains(si.Description, c.id) {
			t.Errorf("%s: description %q does not name the capability", c.id, si.Description)
		}
		if !reflect.DeepEqual(si.Tags, c.wantTags) {
			t.Errorf("%s: tags = %q, want %q", c.id, si.Tags, c.wantTags)
		}
	}
}

// What a provider declares wins; what it leaves empty is filled in; what
// exceeds the card limits is cut to fit rather than failing the card.
func TestSkillInfoOfPrefersTheProviderAndFillsGaps(t *testing.T) {
	long := strings.Repeat("字", MaxSkillNameBytes) // 3 bytes each
	manyTags := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		manyTags = append(manyTags, string(rune('a'+i)))
	}
	p := &described{fake: fake{id: "svc"}, info: map[string]SkillInfo{
		"text.digest": {Name: "Text digest", Description: "SHA-256 of text.",
			Tags: []string{"hash", " ", "hash", "text"}, Examples: []string{`{"text":"hi"}`, ""}},
		"only.name": {Name: "Only a name"},
		"too.long":  {Name: long, Tags: manyTags},
	}}

	si := SkillInfoOf(p, "text.digest")
	if si.Name != "Text digest" || si.Description != "SHA-256 of text." {
		t.Errorf("declared fields were not kept: %+v", si)
	}
	if !reflect.DeepEqual(si.Tags, []string{"hash", "text"}) {
		t.Errorf("tags = %q: empty and duplicate tags must be dropped", si.Tags)
	}
	if !reflect.DeepEqual(si.Examples, []string{`{"text":"hi"}`}) {
		t.Errorf("examples = %q", si.Examples)
	}

	si = SkillInfoOf(p, "only.name")
	if si.Name != "Only a name" || si.Description == "" || !reflect.DeepEqual(si.Tags, []string{"only", "name"}) {
		t.Errorf("gaps were not filled from the id: %+v", si)
	}

	si = SkillInfoOf(p, "too.long")
	if len(si.Name) > MaxSkillNameBytes || !utf8.ValidString(si.Name) {
		t.Errorf("name is %d bytes (valid UTF-8 %v), limit %d", len(si.Name), utf8.ValidString(si.Name), MaxSkillNameBytes)
	}
	if len(si.Tags) != MaxSkillTags {
		t.Errorf("%d tags, want the first %d", len(si.Tags), MaxSkillTags)
	}

	// A capability the provider does not describe falls back entirely.
	si = SkillInfoOf(p, "not.described")
	if si.Name != "not.described" {
		t.Errorf("undescribed capability: %+v", si)
	}
}

// At the voucher door CallerAID is the payer the hub attested, not an
// authenticated caller; VerifiedCaller must not hand it out there, nor on a
// local surface where there is no caller at all.
func TestVerifiedCallerIsEmptyAtTheVoucherDoor(t *testing.T) {
	cases := []struct {
		via  string
		want string
	}{
		{ViaRelay, "aid-caller"},
		{ViaVoucher, ""},
		{"", ""},
		{"something-else", ""},
	}
	for _, c := range cases {
		got := Call{CallerAID: "aid-caller", Via: c.via}.VerifiedCaller()
		if got != c.want {
			t.Errorf("Via=%q: VerifiedCaller() = %q, want %q", c.via, got, c.want)
		}
	}
}
