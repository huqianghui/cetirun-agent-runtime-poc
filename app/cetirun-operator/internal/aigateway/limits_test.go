// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package aigateway

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func limit(value int64) *int64 { return &value }

// liteLLMKeyStore emulates how LiteLLM 1.105 stores a key's limits, as read
// in its key management endpoints: max_parallel_requests, rpm_limit and
// tpm_limit are key columns where an explicit null in /key/update clears the
// value; model_rpm_limit and model_tpm_limit are folded into the key's
// metadata on /key/generate and /key/update, where a null leaves the stored
// map unchanged and any map, empty included, replaces it; a metadata field in
// /key/update replaces the stored metadata wholesale.
type liteLLMKeyStore struct {
	t   *testing.T
	row map[string]any
}

func (s *liteLLMKeyStore) handle(r recordedRequest) (int, string) {
	switch r.path {
	case "/key/generate":
		s.row = map[string]any{"key_alias": r.body["key_alias"], "models": r.body["models"],
			"max_parallel_requests": nil, "rpm_limit": nil, "tpm_limit": nil}
		metadata := map[string]any{}
		if stored, ok := r.body["metadata"].(map[string]any); ok {
			for name, value := range stored {
				metadata[name] = value
			}
		}
		for _, field := range []string{"max_parallel_requests", "rpm_limit", "tpm_limit"} {
			if value, ok := r.body[field]; ok {
				s.row[field] = value
			}
		}
		for _, field := range []string{"model_rpm_limit", "model_tpm_limit"} {
			if value, ok := r.body[field]; ok && value != nil {
				metadata[field] = value
			}
		}
		s.row["metadata"] = metadata
	case "/key/update":
		metadata, _ := s.row["metadata"].(map[string]any)
		if replaced, ok := r.body["metadata"]; ok {
			metadata, _ = replaced.(map[string]any)
		}
		for field, value := range r.body {
			switch field {
			case "key":
			case "model_rpm_limit", "model_tpm_limit":
				if value != nil {
					metadata[field] = value
				}
			case "metadata":
			default:
				s.row[field] = value
			}
		}
		s.row["metadata"] = metadata
	case "/key/info":
		raw, err := json.Marshal(map[string]any{"info": s.row})
		if err != nil {
			s.t.Fatal(err)
		}
		return http.StatusOK, string(raw)
	default:
		s.t.Fatalf("unexpected request %+v", r)
	}
	return http.StatusOK, `{}`
}

func TestKeyLimitsRoundTripThroughLiteLLMStorage(t *testing.T) {
	store := &liteLLMKeyStore{t: t}
	client, requests := testServer(t, store.handle)
	ctx := context.Background()
	desired := KeyLimits{
		MaxParallelRequests: limit(4), RPM: limit(60), TPM: limit(200000),
		ModelRPM: map[string]int64{"example-model": 10}, ModelTPM: map[string]int64{"example-model": 50000, "auto": 8192},
	}
	if err := client.GenerateKey(ctx, GenerateRequest{
		Key: "sk-value", Alias: "cetirun-a", Models: []string{"group-a"},
		Metadata: map[string]string{"cetirun_agent": "a", "cetirun_team": "t"}, Limits: desired,
	}); err != nil {
		t.Fatal(err)
	}
	generated := (*requests)[0].body
	if generated["max_parallel_requests"] != float64(4) || generated["rpm_limit"] != float64(60) || generated["tpm_limit"] != float64(200000) ||
		!reflect.DeepEqual(generated["model_rpm_limit"], map[string]any{"example-model": float64(10)}) {
		t.Fatalf("generate body = %v", generated)
	}
	key, err := client.GetKey(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if !key.Limits.Equal(desired) {
		t.Fatalf("read-back limits = %+v, want %+v", key.Limits, desired)
	}

	changed := KeyLimits{RPM: limit(30), ModelTPM: map[string]int64{"other-model": 4096}}
	if err := client.UpdateKeyLimits(ctx, "abc", changed); err != nil {
		t.Fatal(err)
	}
	if key, err = client.GetKey(ctx, "abc"); err != nil || !key.Limits.Equal(changed) {
		t.Fatalf("read-back after update = %+v, %v; want %+v", key.Limits, err, changed)
	}
	if err := client.UpdateKeyLimits(ctx, "abc", KeyLimits{}); err != nil {
		t.Fatal(err)
	}
	if key, err = client.GetKey(ctx, "abc"); err != nil || !key.Limits.IsZero() {
		t.Fatalf("read-back after clearing = %+v, %v; want no limits", key.Limits, err)
	}
	metadata := store.row["metadata"].(map[string]any)
	if metadata["cetirun_agent"] != "a" || metadata["cetirun_team"] != "t" {
		t.Fatalf("limit updates must keep the key's other metadata, got %v", metadata)
	}
	for _, request := range (*requests)[1:] {
		if _, found := request.body["metadata"]; found {
			t.Fatalf("a limit update must never send metadata, which LiteLLM replaces wholesale: %v", request.body)
		}
	}
}

func TestUpdateKeyLimitsSendsEveryLimitExplicitly(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) { return 200, `{}` })
	if err := client.UpdateKeyLimits(context.Background(), "abc", KeyLimits{TPM: limit(8192), ModelRPM: map[string]int64{"m": 3}}); err != nil {
		t.Fatal(err)
	}
	body := (*requests)[0].body
	want := map[string]any{
		"key": "abc", "max_parallel_requests": nil, "rpm_limit": nil, "tpm_limit": float64(8192),
		"model_rpm_limit": map[string]any{"m": float64(3)}, "model_tpm_limit": map[string]any{},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("update body = %v, want %v", body, want)
	}
}

func TestGenerateKeyWithoutLimitsSendsNoLimitFields(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) { return 200, `{}` })
	if err := client.GenerateKey(context.Background(), GenerateRequest{Key: "sk-v", Alias: "a", Models: []string{NoModels}}); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"max_parallel_requests", "rpm_limit", "tpm_limit", "model_rpm_limit", "model_tpm_limit"} {
		if _, found := (*requests)[0].body[field]; found {
			t.Fatalf("generate without limits sent %s: %v", field, (*requests)[0].body)
		}
	}
}

func TestLimitsRejectZeroAndNegativeValues(t *testing.T) {
	client, requests := testServer(t, func(recordedRequest) (int, string) { return 200, `{}` })
	for _, limits := range []KeyLimits{
		{MaxParallelRequests: limit(0)}, {RPM: limit(-1)}, {TPM: limit(0)},
		{ModelRPM: map[string]int64{"m": 0}}, {ModelTPM: map[string]int64{"m": -5}}, {ModelRPM: map[string]int64{" ": 1}},
	} {
		if err := client.UpdateKeyLimits(context.Background(), "abc", limits); err == nil {
			t.Errorf("UpdateKeyLimits accepted %+v", limits)
		}
		if err := client.GenerateKey(context.Background(), GenerateRequest{Key: "sk-v", Alias: "a", Models: []string{NoModels}, Limits: limits}); err == nil {
			t.Errorf("GenerateKey accepted %+v", limits)
		}
	}
	if len(*requests) != 0 {
		t.Fatalf("no request may be sent, got %d", len(*requests))
	}
}

func TestGetKeyReadsStoredLimitShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		info string
		want KeyLimits
	}{
		{name: "never set", info: `{"key_alias":"a"}`},
		{name: "nulls and cleared maps", info: `{"max_parallel_requests":null,"rpm_limit":null,"tpm_limit":null,"metadata":{"model_rpm_limit":{},"model_tpm_limit":null}}`},
		{name: "null metadata", info: `{"metadata":null}`},
		{name: "values", info: `{"max_parallel_requests":2,"rpm_limit":9000000000,"tpm_limit":4096.0,"metadata":{"cetirun_agent":"a","model_rpm_limit":{"m":5}}}`,
			want: KeyLimits{MaxParallelRequests: limit(2), RPM: limit(9000000000), TPM: limit(4096), ModelRPM: map[string]int64{"m": 5}}},
		{name: "unreadable values", info: `{"rpm_limit":0,"tpm_limit":"many","metadata":{"model_rpm_limit":{"m":1.5},"model_tpm_limit":[1]}}`,
			want: KeyLimits{RPM: limit(invalidLimit), TPM: limit(invalidLimit), ModelRPM: map[string]int64{"m": invalidLimit}, ModelTPM: map[string]int64{"": invalidLimit}}},
		{name: "metadata is not an object", info: `{"metadata":"text"}`,
			want: KeyLimits{ModelRPM: map[string]int64{"": invalidLimit}, ModelTPM: map[string]int64{"": invalidLimit}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := testServer(t, func(recordedRequest) (int, string) { return 200, `{"info":` + tc.info + `}` })
			key, err := client.GetKey(context.Background(), "abc")
			if err != nil {
				t.Fatal(err)
			}
			if !key.Limits.Equal(tc.want) {
				t.Fatalf("limits = %+v, want %+v", key.Limits, tc.want)
			}
			if tc.name == "unreadable values" && key.Limits.Equal(KeyLimits{}) {
				t.Fatal("an unreadable stored limit must not read as unlimited")
			}
		})
	}
}

func TestKeyLimitsEqualTreatsEmptyMapsAsUnset(t *testing.T) {
	if !(KeyLimits{ModelRPM: map[string]int64{}}).Equal(KeyLimits{}) || !(KeyLimits{}).IsZero() {
		t.Fatal("an empty per-model map must equal an absent one")
	}
	a := KeyLimits{RPM: limit(1), ModelTPM: map[string]int64{"m": 4096}}
	for _, b := range []KeyLimits{{RPM: limit(2), ModelTPM: map[string]int64{"m": 4096}}, {ModelTPM: map[string]int64{"m": 4096}},
		{RPM: limit(1), ModelTPM: map[string]int64{"n": 4096}}, {RPM: limit(1), ModelRPM: map[string]int64{"m": 4096}}} {
		if a.Equal(b) || b.Equal(a) {
			t.Errorf("%+v must differ from %+v", a, b)
		}
	}
}
