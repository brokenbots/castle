package store

import (
	"bytes"
	"encoding/json"
	"testing"
)

// rawNUL is an event payload carrying a bare 0x00 byte inside a JSON string:
// what a non-protojson writer (or a legacy sqlite row) may contain.
const rawNUL = `{"step":"s1","chunk":"a` + "\x00" + `b"}`

func TestNormalizeEventPayload(t *testing.T) {
	const escapeNUL = `{"chunk":"a\u0000b"}`

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "protojson nul escape becomes ufffd escape",
			in:   escapeNUL,
			want: `{"chunk":"a\ufffdb"}`,
		},
		{
			name: "raw nul byte becomes ufffd escape",
			in:   rawNUL,
			want: `{"step":"s1","chunk":"a\ufffdb"}`,
		},
		{
			name: "nul escape is replaced inside nested strings too",
			in:   `{"a":{"b":["x\u0000y"]}}`,
			want: `{"a":{"b":["x\ufffdy"]}}`,
		},
		{
			// protojson output is exactly this shape: escaped control
			// characters that are not NUL must survive byte-for-byte so
			// the sqlite profile keeps its exact protojson text.
			name: "other control escapes copied verbatim",
			in:   "{\"k\":\"x\\u001fy\\t\\nz\"}",
			want: "{\"k\":\"x\\u001fy\\t\\nz\"}",
		},
		{
			name: "raw control byte inside string becomes escape, not ufffd",
			in:   `{"k":"x` + "\x1f" + `y"}`,
			want: `{"k":"x\u001fy"}`,
		},
		{
			name: "raw nul and raw control inside string",
			in:   `{"k":"` + "\x00\x1f\x00" + `"}`,
			want: `{"k":"\ufffd\u001f\ufffd"}`,
		},
		{
			name: "ordinary escapes copied verbatim",
			in:   `{"k":"a\\b\"c\/d"}`,
			want: `{"k":"a\\b\"c\/d"}`,
		},
		{
			// A scanner that ignores string context would corrupt legal
			// JSON whitespace between tokens.
			name: "string-structure whitespace copied verbatim",
			in:   "{\n\t\"k\"\r: [ 1 , 2 ]\n}",
			want: "{\n\t\"k\"\r: [ 1 , 2 ]\n}",
		},
		{
			name: "u escape that is not nul copied verbatim",
			in:   `{"k":"a\u0041b"}`,
			want: `{"k":"a\u0041b"}`,
		},
		{
			name: "truncated nul escape copied verbatim",
			in:   `{"k":"a\u00"}`,
			want: `{"k":"a\u00"}`,
		},
		{
			name: "non-ascii utf8 copied verbatim",
			in:   `{"k":"aé日b"}`,
			want: `{"k":"aé日b"}`,
		},
		{
			name: "empty payload unaffected",
			in:   "",
			want: "",
		},
		{
			// A NUL outside any JSON string is malformed JSON either way:
			// the scanner has no string context, so it copies verbatim.
			name: "nul outside strings copied verbatim",
			in:   `{"k":1}` + "\x00",
			want: `{"k":1}` + "\x00",
		},
		{
			name: "escaped backslash does not close string",
			in:   `{"k":"a\\"` + "\x00" + `}`,
			want: `{"k":"a\\"` + "\x00" + `}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeEventPayload([]byte(tc.in))
			if !bytes.Equal(got, []byte(tc.want)) {
				t.Fatalf("NormalizeEventPayload(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Every output is valid JSON when its input is (the malformed
			// trailing-NUL cases are malformed in and out by design).
			if json.Valid([]byte(tc.in)) && !json.Valid(got) {
				t.Fatalf("output not valid JSON: %q", got)
			}
			// Idempotent: normalizing twice equals normalizing once.
			if again := NormalizeEventPayload(got); !bytes.Equal(again, got) {
				t.Fatalf("not idempotent: second pass = %q, want %q", again, got)
			}
		})
	}
}

// TestNormalizeEventPayloadDecodesAcrossEngines pins the actual contract the
// engines share: after normalization the escaped and raw forms decode to the
// same string carrying U+FFFD, and the replacement character is the only
// observable difference from NUL.
func TestNormalizeEventPayloadDecodesAcrossEngines(t *testing.T) {
	decoded := func(doc []byte) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(doc, &m); err != nil {
			t.Fatalf("unmarshal %q: %v", doc, err)
		}
		return m
	}
	esc := decoded(NormalizeEventPayload([]byte(`{"chunk":"a\u0000b"}`)))
	raw := decoded(NormalizeEventPayload([]byte(rawNUL)))
	for name, m := range map[string]map[string]any{"escaped": esc, "raw": raw} {
		if m["chunk"] != "a\uFFFDb" {
			t.Fatalf("%s: decoded chunk = %q, want a\uFFFDb", name, m["chunk"])
		}
	}
}
