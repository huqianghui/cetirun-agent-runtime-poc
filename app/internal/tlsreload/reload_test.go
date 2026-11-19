// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tlsreload

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newAuthority(t *testing.T) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	raw, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ = x509.ParseCertificate(raw)
	return authority{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})}
}
func leaf(t *testing.T, ca authority, name string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	raw, err := x509.CreateCertificate(rand.Reader, cert, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}
func save(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func material(t *testing.T, ca authority, name string) (*Reloader, Files) {
	t.Helper()
	dir := t.TempDir()
	files := Files{filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt")}
	crt, key := leaf(t, ca, name)
	save(t, files.Certificate, crt)
	save(t, files.Key, key)
	save(t, files.CA, ca.pem)
	r, err := New(files)
	if err != nil {
		t.Fatal(err)
	}
	return r, files
}
func reload(t *testing.T, r *Reloader) {
	t.Helper()
	if changed, err := r.Reload(); err != nil || !changed {
		t.Fatalf("reload: changed=%v err=%v", changed, err)
	}
}
func rotate(t *testing.T, r *Reloader, files Files, ca authority, name string) {
	t.Helper()
	crt, key := leaf(t, ca, name)
	save(t, files.Certificate, crt)
	save(t, files.Key, key)
	reload(t, r)
}
func fixture(t *testing.T, server, client *Reloader) (*http.Client, string) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.TLS.PeerCertificates[0].Subject.CommonName)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = server.ServerConfig(true)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	transport := client.ClientTransport()
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: time.Second}, srv.URL
}
func get(client *http.Client, url string) (string, string, error) {
	response, err := client.Get(url)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return response.TLS.PeerCertificates[0].Subject.CommonName, string(body), err
}
func TestTLSRotationAndInvalidUpdate(t *testing.T) {
	ca := newAuthority(t)
	server, sf := material(t, ca, "server-one")
	client, cf := material(t, ca, "client-one")
	httpClient, url := fixture(t, server, client)
	check := func(wantServer, wantClient string) {
		t.Helper()
		s, c, err := get(httpClient, url)
		if err != nil || s != wantServer || c != wantClient {
			t.Fatalf("TLS peers %s/%s, %v", s, c, err)
		}
	}
	check("server-one", "client-one")
	rotate(t, server, sf, ca, "server-two")
	rotate(t, client, cf, ca, "client-two")
	check("server-two", "client-two")
	save(t, sf.Key, []byte("invalid-private-material"))
	if _, err := server.Reload(); err == nil {
		t.Fatal("invalid update accepted")
	}
	httpClient.CloseIdleConnections()
	check("server-two", "client-two")
	server.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	httpClient.CloseIdleConnections()
	if _, _, err := get(httpClient, url); err == nil {
		t.Fatal("expired server snapshot accepted")
	}
	client.now = server.now
	if _, _, err := get(httpClient, url); err == nil {
		t.Fatal("expired client snapshot accepted")
	}
}
func TestCARolloverAndRemoval(t *testing.T) {
	a, b := newAuthority(t), newAuthority(t)
	server, sf := material(t, a, "server-a")
	client, cf := material(t, a, "client-a")
	httpClient, url := fixture(t, server, client)
	for _, pair := range []struct {
		r *Reloader
		f Files
	}{{server, sf}, {client, cf}} {
		save(t, pair.f.CA, append(append([]byte{}, a.pem...), b.pem...))
		reload(t, pair.r)
	}
	rotate(t, server, sf, b, "server-b")
	rotate(t, client, cf, b, "client-b")
	if _, _, err := get(httpClient, url); err != nil {
		t.Fatal(err)
	}
	save(t, sf.CA, b.pem)
	reload(t, server)
	save(t, cf.CA, b.pem)
	reload(t, client)
	if _, _, err := get(httpClient, url); err != nil {
		t.Fatal(err)
	}
	old, oldFiles := material(t, a, "client-old")
	oldClient, _ := fixture(t, server, old)
	// The old client must trust the new server so failure proves client-CA removal.
	save(t, oldFiles.CA, b.pem)
	reload(t, old)
	if _, _, err := get(oldClient, url); err == nil {
		t.Fatal("removed client CA remains trusted")
	}
	rotate(t, server, sf, a, "server-old")
	httpClient.CloseIdleConnections()
	if _, _, err := get(httpClient, url); err == nil {
		t.Fatal("removed server CA remains trusted")
	}
}
func TestConcurrentReloadAndRequests(t *testing.T) {
	ca := newAuthority(t)
	server, sf := material(t, ca, "server")
	client, cf := material(t, ca, "client")
	httpClient, url := fixture(t, server, client)
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < 20; j++ {
				if _, _, err := get(httpClient, url); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		rotate(t, server, sf, ca, fmt.Sprintf("server-%d", i))
		rotate(t, client, cf, ca, fmt.Sprintf("client-%d", i))
	}
	group.Wait()
}
func TestStartupValidationAndCancellation(t *testing.T) {
	ca := newAuthority(t)
	r, f := material(t, ca, "server")
	save(t, f.CA, []byte("not a CA"))
	if _, err := New(f); err == nil {
		t.Fatal("invalid initial trust accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reload loop did not stop")
	}
}
func TestProjectedSecretGeneration(t *testing.T) {
	ca := newAuthority(t)
	dir := t.TempDir()
	for _, name := range []string{"one", "two"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		crt, key := leaf(t, ca, name)
		save(t, filepath.Join(dir, name, "tls.crt"), crt)
		save(t, filepath.Join(dir, name, "tls.key"), key)
		save(t, filepath.Join(dir, name, "ca.crt"), ca.pem)
	}
	if err := os.Symlink("one", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tls.crt", "tls.key", "ca.crt"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	f := Files{filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt")}
	r, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("two", filepath.Join(dir, "..next")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "..next"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	reload(t, r)
	if r.current.Load().certificate.Leaf.Subject.CommonName != "two" {
		t.Fatal("new generation not selected")
	}
}
