package imap

import (
	"reflect"
	"testing"
)

func TestDecodeHeader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "hello", want: "hello"},
		{name: "utf8 base64", in: "=?UTF-8?B?4pyT?=", want: "✓"},
		{name: "adjacent words", in: "=?UTF-8?Q?hello?= \t =?UTF-8?Q?world?=", want: "helloworld"},
		{name: "latin1", in: "=?ISO-8859-1?Q?caf=E9?=", want: "café"},
		{name: "windows1252", in: "=?windows-1252?Q?=80?=", want: "€"},
		{name: "language tag", in: "=?UTF-8*de?Q?Gr=C3=BC=C3=9Fe?=", want: "Grüße"},
		{name: "unknown charset", in: "=?x-unknown?Q?hello?=", want: "=?x-unknown?Q?hello?="},
		{name: "malformed", in: "prefix =?UTF-8?Q?unterminated", want: "prefix =?UTF-8?Q?unterminated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecodeHeader(tt.in); got != tt.want {
				t.Errorf("DecodeHeader(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDecodeParams(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{name: "nil", in: nil, want: nil},
		{name: "plain and lowercase", in: map[string]string{"CHARSET": "utf-8"}, want: map[string]string{"charset": "utf-8"}},
		{name: "single extended", in: map[string]string{"filename*": "utf-8'en'%E2%82%AC.txt"}, want: map[string]string{"filename": "€.txt"}},
		{name: "plain continuation", in: map[string]string{"name*0": "long ", "name*1": "value"}, want: map[string]string{"name": "long value"}},
		{name: "encoded continuation", in: map[string]string{"name*0*": "utf-8''%C2%A3", "name*1*": "%2Etxt"}, want: map[string]string{"name": "£.txt"}},
		{name: "extended wins fallback", in: map[string]string{"filename": "fallback.txt", "filename*": "utf-8''actual.txt"}, want: map[string]string{"filename": "actual.txt"}},
		{name: "continuation wins fallback", in: map[string]string{"name": "fallback", "name*0": "long ", "name*1": "value"}, want: map[string]string{"name": "long value"}},
		{name: "gap stops assembly", in: map[string]string{"name*0": "first", "name*2": "third"}, want: map[string]string{"name": "first"}},
		{name: "malformed percent kept", in: map[string]string{"name*": "utf-8''a%ZZb"}, want: map[string]string{"name": "a%ZZb"}},
		{name: "latin1", in: map[string]string{"name*": "iso-8859-1''caf%E9"}, want: map[string]string{"name": "café"}},
		{name: "unknown charset bytes", in: map[string]string{"name*": "x-unknown''caf%E9"}, want: map[string]string{"name": "caf\xe9"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecodeParams(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DecodeParams(%v) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseMessageIDList(t *testing.T) {
	if got, want := ParseMessageIDList("phrase <one@example> noise <two@example>"), []string{"<one@example>", "<two@example>"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseMessageIDList() = %v, want %v", got, want)
	}
	if got := ParseMessageIDList("<unterminated"); got != nil {
		t.Fatalf("ParseMessageIDList(unterminated) = %v, want nil", got)
	}
}
