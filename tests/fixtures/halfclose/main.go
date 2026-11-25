// Test client for the mTLS listener, including TLS close_notify half-close.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 5 {
		panic("expected address CA client-cert client-key")
	}
	roots := x509.NewCertPool()
	raw, err := os.ReadFile(os.Args[2])
	if err != nil || !roots.AppendCertsFromPEM(raw) {
		panic("bad CA")
	}
	pair, err := tls.LoadX509KeyPair(os.Args[3], os.Args[4])
	if err != nil {
		panic(err)
	}
	c, err := tls.Dial("tcp", os.Args[1], &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	if err != nil {
		panic(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	data := bytes.Repeat([]byte("half-close\x00"), 8192)
	if _, err = c.Write(data); err != nil {
		panic(err)
	}
	if err = c.CloseWrite(); err != nil {
		panic(err)
	}
	reply, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(reply, data) {
		panic(fmt.Sprintf("half-close reply length %d, error %v", len(reply), err))
	}
	fmt.Printf("echoed %d bytes after TLS half-close\n", len(reply))
}
