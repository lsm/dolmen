package value

import "testing"

func TestFindJSONNUL(t *testing.T) {
	cases := []struct {
		doc    string
		field  string
		inName bool
		found  bool
	}{
		{`{"namespace":"a","records":[{"body":"x\u0000y"}]}`, "body", false, true},
		{`{"table":"a\u0000b"}`, "table", false, true},
		{`{"args":["ok","a\u0000"]}`, "args", false, true},
		{`{"records":[{"prefs":{"k\u0000":1}}]}`, "", true, true},
		{`{"body":"literal \\u0000 text"}`, "", false, false},
		{`{"body":"\\\u0000"}`, "body", false, true},
		{`{"a":"x","b":["\"",{"c":"fine"}]}`, "", false, false},
	}
	for _, c := range cases {
		field, inName, found := FindJSONNUL([]byte(c.doc))
		if field != c.field || inName != c.inName || found != c.found {
			t.Fatalf("%s: got (%q,%v,%v), want (%q,%v,%v)", c.doc, field, inName, found, c.field, c.inName, c.found)
		}
	}
}
