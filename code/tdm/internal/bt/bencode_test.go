package bt

import (
	"testing"
)

func TestBencodeRoundtrip(t *testing.T) {
	cases := []struct {
		raw  string
		want any
	}{
		{"i42e", int64(42)},
		{"i-7e", int64(-7)},
		{"5:hello", "hello"},
		{"0:", ""},
		{"l4:spami42ee", []any{"spam", int64(42)}},
		{"d3:bar4:spam3:fooi42ee", map[string]any{"bar": "spam", "foo": int64(42)}},
		{"le", []any{}},
	}
	for _, c := range cases {
		v, err := Decode([]byte(c.raw))
		if err != nil {
			t.Fatalf("Decode(%q): %v", c.raw, err)
		}
		_ = v
	}
	// roundtrip
	m := map[string]any{"name": int64(4), "pieces": "xx", "list": []any{int64(1), int64(2)}}
	enc := string(Encode(m))
	dec, err := Decode([]byte(enc))
	if err != nil {
		t.Fatalf("roundtrip decode: %v", err)
	}
	m2, ok := dec.(map[string]any)
	if !ok || m2["name"] != int64(4) || len(m2["list"].([]any)) != 2 {
		t.Fatalf("roundtrip mismatch: %#v", m2)
	}
}
