// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/registry"
)

func TestWithoutURLQueries(t *testing.T) {
	for in, want := range map[string]string{
		`Get "https://storage.example.com/data/blob?a=1&b=2": read: connection reset by peer`: `Get "https://storage.example.com/data/blob": read: connection reset by peer`,
		`GET "http://127.0.0.1:5000/data?a=1": response status code 403: Forbidden`:           `GET "http://127.0.0.1:5000/data": response status code 403: Forbidden`,
		`fetch https://storage.example.com/data?a=1 failed`:                                   `fetch https://storage.example.com/data failed`,
		`see https://storage.example.com/page#part`:                                           `see https://storage.example.com/page`,
		`locate OCI chart oci://registry.example.com/helm/cetirun:1.0.0: not found`:           `locate OCI chart oci://registry.example.com/helm/cetirun:1.0.0: not found`,
	} {
		if got := withoutURLQueries(in); got != want {
			t.Errorf("withoutURLQueries(%q) = %q, want %q", in, got, want)
		}
	}
}

// redirectingRegistry serves a chart manifest and redirects blob downloads
// to a storage path that refuses them, as a registry whose storage fails
// does. It returns the registry's host and port.
func redirectingRegistry(t *testing.T) string {
	t.Helper()
	digest := func(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }
	config, layer := []byte("{}"), []byte("synthetic chart")
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": registry.ConfigMediaType, "digest": digest(config), "size": len(config)},
		"layers":        []any{map[string]any{"mediaType": registry.ChartLayerMediaType, "digest": digest(layer), "size": len(layer)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/helm/cetirun/manifests/"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digest(manifest))
			w.Header().Set("Content-Length", strconv.Itoa(len(manifest)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(manifest)
			}
		case strings.HasPrefix(r.URL.Path, "/v2/helm/cetirun/blobs/"):
			http.Redirect(w, r, "/storage/"+path.Base(r.URL.Path)+"?query=synthetic", http.StatusTemporaryRedirect)
		case strings.HasPrefix(r.URL.Path, "/storage/"):
			http.Error(w, "denied", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

// A failed chart download is a ChartDownloadError. Its message keeps the
// cause, which quotes the storage URL the registry redirected to, without
// that URL's query string.
func TestChartDownloadErrorKeepsTheCauseWithoutURLQueries(t *testing.T) {
	client := NewClient(ChartReference{RepoURL: "oci://" + redirectingRegistry(t) + "/helm/cetirun", Version: "1.0.0"})
	_, err := client.InspectChart(context.Background(), "")
	var download *ChartDownloadError
	if !errors.As(err, &download) {
		t.Fatalf("a failed download must be a ChartDownloadError, got %T: %v", err, err)
	}
	if download.Version != "1.0.0" || !strings.HasSuffix(download.Chart, "/helm/cetirun:1.0.0") {
		t.Fatalf("chart = %q, version = %q", download.Chart, download.Version)
	}
	if !strings.Contains(download.Err.Error(), "?query=synthetic") {
		t.Fatalf("the registry client's error no longer quotes the storage URL, so this test proves nothing: %v", download.Err)
	}
	message := err.Error()
	if strings.Contains(message, "query=synthetic") || !strings.Contains(message, "/storage/") || !strings.Contains(message, "403") {
		t.Fatalf("message = %q, want the cause without the URL query", message)
	}
}
