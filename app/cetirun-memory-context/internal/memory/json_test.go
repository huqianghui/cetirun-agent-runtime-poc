// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import "testing"

func TestStrictJSON(t *testing.T) {
	for _, input := range []string{`{"id":"a","id":"b"}`, `{"ID":"a"}`, `{"revision":1.0}`, `{"revision":1e1}`, `{"id":"a"} {}`, `{"extra":true}`} {
		var ref Ref
		if err := Decode([]byte(input), &ref); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	var ref Ref
	if err := Decode([]byte(`{"id":"a","revision":1,"digest":"b"}`), &ref); err != nil {
		t.Fatal(err)
	}
}
