// nix-fetch-fixture implements the GitHub Enterprise endpoints used by Nix's
// github: fetcher. It runs in Docker, with no public network dependencies.
package main

import (
	"archive/tar"
	"compress/gzip"
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
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) == 2 {
		client := &http.Client{Timeout: 5 * time.Second}
		res, err := client.Post("http://127.0.0.1:8080/"+os.Args[1], "text/plain", nil)
		if err != nil {
			log.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			log.Fatal(res.Status)
		}
		if _, err := io.Copy(os.Stdout, res.Body); err != nil {
			log.Fatal(err)
		}
		return
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Pulse Nix fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Fatal(err)
	}
	for _, addr := range addrs {
		if ip, _, err := net.ParseCIDR(addr.String()); err == nil {
			cert.IPAddresses = append(cert.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("/tmp/nix-fetch-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		log.Fatal(err)
	}
	var mu sync.Mutex
	state := struct {
		Revision, Branches, Archives int
		Disabled                     bool
	}{Revision: 1}
	control := http.NewServeMux()
	control.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/advance":
			state.Revision++
		case "/disable":
			state.Disabled = true
		case "/enable":
			state.Disabled = false
		case "/stats":
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(state)
	})
	go func() { log.Fatal(http.ListenAndServe("127.0.0.1:8080", control)) }()
	api := http.NewServeMux()
	api.HandleFunc("/api/v3/repos/colony/probe/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rev, disabled := state.Revision, state.Disabled
		branch := strings.HasSuffix(r.URL.Path, "/commits/main")
		archive := strings.Contains(r.URL.Path, "/tarball/")
		if branch {
			state.Branches++
		} else if archive {
			state.Archives++
		}
		mu.Unlock()
		if disabled {
			http.Error(w, "upstream unavailable", 503)
			return
		}
		if branch {
			// Delay makes simultaneous cold/expired lookups observable.
			time.Sleep(150 * time.Millisecond)
			fmt.Fprintf(w, `{"sha":"%040x","commit":{"tree":{"sha":"%040x"}}}`, rev, rev)
			return
		}
		if !archive {
			http.NotFound(w, r)
			return
		}
		var version int
		if _, err := fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%x", &version); err != nil {
			http.Error(w, "bad revision", 400)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		tw := tar.NewWriter(gz)
		defer tw.Close()
		files := []struct{ name, data string }{
			{"flake.nix", `{
  outputs = { self }: let
    package = builtins.derivation {
      name = "pulse-fetch-probe";
      system = builtins.currentSystem;
      builder = "/bin/sh";
      args = [ "-c" "/bin/mkdir -p $out/bin; /bin/cp ${./program} $out/bin/probe; /bin/chmod +x $out/bin/probe" ];
    };
  in {
    probe = { revision = self.rev; output = package.outPath; };
    packages.${builtins.currentSystem}.default = package;
  };
}`},
			{"program", fmt.Sprintf("#!/bin/sh\necho revision-%d\n", version)},
		}
		for _, f := range files {
			// Keep the flake pure so Nix also exercises its evaluation cache.
			if f.name == "flake.nix" {
				system := "x86_64-linux"
				if runtime.GOARCH == "arm64" {
					system = "aarch64-linux"
				}
				f.data = strings.ReplaceAll(f.data, "builtins.currentSystem", fmt.Sprintf("%q", system))
			}
			if err := tw.WriteHeader(&tar.Header{Name: "source/" + f.name, Mode: 0755, Size: int64(len(f.data)), ModTime: time.Unix(1700000000, 0)}); err != nil {
				return
			}
			if _, err := tw.Write([]byte(f.data)); err != nil {
				return
			}
		}
	})
	listener, err := tls.Listen("tcp", ":443", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(http.Serve(listener, api))
}
