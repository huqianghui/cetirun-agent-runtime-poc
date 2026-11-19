// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"encoding/json"
	"testing"
)

func TestNativeHoldReleaseConfigurationIsExplicitAndBounded(t *testing.T) {
	config := testConfig()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["nativeExecution"]; present {
		t.Fatal("default admission configuration acquired native activation")
	}
	config.NativeExecution = true
	if err := config.Validate(); err == nil {
		t.Fatal("native hold release was enabled without the platform state namespace")
	}
	config.PlatformNamespace, config.PlatformConfigMapName = "operator-test", "platform"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseConfig(string(raw))
	if err != nil || !parsed.NativeExecution {
		t.Fatal("native admission configuration did not round trip:", err)
	}
	fields["nativeExecution"] = json.RawMessage(`"true"`)
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseConfig(string(raw)); err == nil {
		t.Fatal("a non-boolean native activation flag was accepted")
	}
}
