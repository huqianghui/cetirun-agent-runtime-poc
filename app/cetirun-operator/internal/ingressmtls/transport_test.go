// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envoy

package ingressmtls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: randomSerial(t), Subject: pkix.Name{CommonName: "Synthetic test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	raw, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{parsed, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})}
}

func randomSerial(t *testing.T) *big.Int {
	t.Helper()
	value, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func (ca testCA) issue(t *testing.T, name string, client, expired bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: randomSerial(t), Subject: pkix.Name{CommonName: "Synthetic leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if client {
		uri, err := url.Parse(name)
		if err != nil {
			t.Fatal(err)
		}
		cert.URIs, cert.ExtKeyUsage = []*url.URL{uri}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		cert.DNSNames = []string{name}
	}
	if expired {
		cert.NotAfter = time.Now().Add(-time.Minute)
	}
	raw, err := x509.CreateCertificate(rand.Reader, cert, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, certPEM, keyPEM
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func startTestEnvoy(t *testing.T, plan Plan, cert, key, ca []byte, backends map[string]int, wantReady ...bool) (map[string]int, func()) {
	t.Helper()
	binary := os.Getenv("CETIRUN_ENVOY_BINARY")
	if binary == "" {
		t.Fatal("CETIRUN_ENVOY_BINARY must name the separately restored, checksum-verified Envoy 1.36.3 binary")
	}
	version, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil || !bytes.Contains(version, []byte("/1.36.3/")) {
		t.Fatalf("pinned Envoy 1.36.3 is unavailable: %v", err)
	}
	dir := t.TempDir()
	for name, contents := range map[string][]byte{"tls.crt": cert, "tls.key": key, "ca.crt": ca} {
		if err := os.WriteFile(filepath.Join(dir, name), contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	bootstrap, err := plan.Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap = bytes.ReplaceAll(bootstrap, []byte(TLSPath), []byte(dir))
	var config map[string]any
	if err := json.Unmarshal(bootstrap, &config); err != nil {
		t.Fatal(err)
	}
	admin := freePort(t)
	config["admin"].(map[string]any)["address"] = socket("127.0.0.1", int32(admin))
	resources := config["static_resources"].(map[string]any)
	ports := map[string]int{}
	for _, raw := range resources["listeners"].([]any) {
		listener := raw.(map[string]any)
		name := listener["name"].(string)
		ports[name] = freePort(t)
		listener["address"] = socket("127.0.0.1", int32(ports[name]))
	}
	for _, raw := range resources["clusters"].([]any) {
		cluster := raw.(map[string]any)
		endpoint := cluster["load_assignment"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)["lb_endpoints"].([]any)[0].(map[string]any)["endpoint"].(map[string]any)
		endpoint["address"] = socket("127.0.0.1", int32(backends[cluster["name"].(string)]))
	}
	bootstrap, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "envoy.json")
	if err := os.WriteFile(path, bootstrap, 0600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(dir, "envoy.log"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "--disable-hot-restart", "--concurrency", "1", "--log-level", "error", "-c", path)
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := false
	stop := func() {
		if !done {
			done = true
			_ = command.Process.Kill()
			_ = command.Wait()
			_ = logFile.Close()
		}
	}
	t.Cleanup(stop)
	readyClient := &http.Client{Timeout: 200 * time.Millisecond}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		response, err := readyClient.Get(fmt.Sprintf("http://127.0.0.1:%d/ready", admin))
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				probe, err := filepath.Abs("../../../cetirun-chart/configs/ingress-health.cjs")
				if err != nil {
					t.Fatal(err)
				}
				check := exec.Command("node", "-e", `require(process.argv[1])((ready) => process.exit(ready ? 0 : 1), process.argv[2], Number(process.argv[3]), process.argv[4])`,
					probe, dir+string(os.PathSeparator), strconv.Itoa(admin), plan.ServerName)
				output, probeErr := check.CombinedOutput()
				expected := len(wantReady) == 0 || wantReady[0]
				if (probeErr == nil) != expected {
					t.Fatalf("actual endpoint certificate readiness differs: %v %s", probeErr, output)
				}
				if expected {
					if err := os.Rename(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "saved.crt")); err != nil {
						t.Fatal(err)
					}
					missing := exec.Command("node", check.Args[1:]...)
					if err := missing.Run(); err == nil {
						t.Fatal("certificate loss left the real endpoint ready")
					}
					if err := os.Rename(filepath.Join(dir, "saved.crt"), filepath.Join(dir, "tls.crt")); err != nil {
						t.Fatal(err)
					}
				}
				return ports, stop
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("managed Envoy endpoint did not become ready")
	return nil, nil
}

func tlsRequest(port int, serverName string, root []byte, identity *tls.Certificate) (string, error) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(root)
	config := &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS13}
	if identity != nil {
		config.Certificates = []tls.Certificate{*identity}
	}
	transport := &http.Transport{TLSClientConfig: config, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	response, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/", port))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return string(body), err
}

func TestEnvoyFinalHopMTLS(t *testing.T) {
	for _, harness := range []string{"openclaw", "hermes", "custom", "custom-terminal"} {
		t.Run(harness, func(t *testing.T) {
			ca, foreign := newTestCA(t), newTestCA(t)
			plan := Plan{Binding: Binding{Harness: harness, Port: 18789, Terminal: true, Desktop: true, ClientURI: "spiffe://example.com/gateway/agent-uid"},
				ServerName: "agent-uid.example.com"}
			if harness == "custom" {
				plan.Desktop = false
				plan.ChatClients = []ChatClient{{URI: "spiffe://example.com/portal/agent-uid"}}
			}
			if harness == "custom-terminal" {
				plan = plan.TerminalEndpoint()
			}
			serverIdentity, cert, key := ca.issue(t, plan.ServerName, false, false)
			good, _, _ := ca.issue(t, plan.ClientURI, true, false)
			wrongURI, _, _ := ca.issue(t, "spiffe://example.com/gateway/other-agent", true, false)
			wrongCA, _, _ := foreign.issue(t, plan.ClientURI, true, false)
			expired, _, _ := ca.issue(t, plan.ClientURI, true, true)
			portal, _, _ := ca.issue(t, "spiffe://example.com/portal/agent-uid", true, false)
			backends := map[string]int{}
			for _, port := range plan.Ports() {
				name := port.Name
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(writer, harness+"/"+name)
				}))
				t.Cleanup(server.Close)
				_, rawPort, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
				if err != nil {
					t.Fatal(err)
				}

				backends[name], err = strconv.Atoi(rawPort)
				if err != nil {
					t.Fatal(err)
				}
			}
			ports, stop := startTestEnvoy(t, plan, cert, key, ca.pem, backends)
			for name, port := range ports {
				body, err := tlsRequest(port, plan.ServerName, ca.pem, &good)
				if err != nil || body != harness+"/"+name {
					t.Fatalf("%s valid managed peer failed: %v", name, err)
				}
				_, portalError := tlsRequest(port, plan.ServerName, ca.pem, &portal)
				if (portalError == nil) != (harness == "custom") {
					t.Fatal("Portal client authority escaped its explicitly approved Custom HTTP endpoint:", portalError)
				}
				for label, identity := range map[string]*tls.Certificate{
					"missing": nil, "wrong URI": &wrongURI, "wrong CA": &wrongCA, "expired": &expired,
					"wrong client EKU": &serverIdentity,
				} {
					if _, err := tlsRequest(port, plan.ServerName, ca.pem, identity); err == nil {
						t.Fatalf("%s accepted %s client", name, label)
					}
				}
				for label, pair := range map[string]struct {
					name string
					root []byte
				}{"wrong server SAN": {"other.example.com", ca.pem}, "wrong server CA": {plan.ServerName, foreign.pem}} {
					if _, err := tlsRequest(port, pair.name, pair.root, &good); err == nil {
						t.Fatalf("%s accepted %s", name, label)
					}
				}
				client := &http.Client{Timeout: time.Second}
				response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
				if err == nil {
					response.Body.Close()
					t.Fatal("TLS endpoint accepted direct plaintext")
				}
			}
			stop()
			for _, port := range ports {
				if _, err := tlsRequest(port, plan.ServerName, ca.pem, &good); err == nil {
					t.Fatal("endpoint continued serving after lifecycle cleanup")
				}
			}
			// Rotation is a coordinated endpoint replacement, not a hot reload.
			newCA := newTestCA(t)
			_, newCert, newKey := newCA.issue(t, plan.ServerName, false, false)
			next, _, _ := newCA.issue(t, plan.ClientURI, true, false)
			rotated, _ := startTestEnvoy(t, plan, newCert, newKey, newCA.pem, backends)
			for _, port := range rotated {
				if _, err := tlsRequest(port, plan.ServerName, newCA.pem, &good); err == nil {
					t.Fatal("retired client authority survived coordinated rotation")
				}
				if _, err := tlsRequest(port, plan.ServerName, newCA.pem, &next); err != nil {
					t.Fatal("rotated managed peer failed:", err)
				}
			}
			_, expiredCert, expiredKey := ca.issue(t, plan.ServerName, false, true)
			expiredPorts, _ := startTestEnvoy(t, plan, expiredCert, expiredKey, ca.pem, backends, false)
			for _, port := range expiredPorts {
				if _, err := tlsRequest(port, plan.ServerName, ca.pem, &good); err == nil {
					t.Fatal("Gateway accepted an expired Agent serving certificate")
				}
			}
		})
	}
}
