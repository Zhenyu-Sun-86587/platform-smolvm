// A disposable Guest-side oracle for gateway retries and credential stripping.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
)

func main() {
	go func() {
		l, err := net.Listen("tcp", ":8081")
		if err != nil {
			log.Fatal(err)
		}
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				data, err := io.ReadAll(io.LimitReader(conn, 1<<20))
				if err == nil {
					_, _ = conn.Write(data)
				}
			}()
		}
	}()
	var n atomic.Int32
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "credential leaked", 500)
			return
		}
		if r.URL.Path == "/apply-and-drop" && r.Method == "POST" {
			n.Add(1)
			c, _, e := w.(http.Hijacker).Hijack()
			if e == nil {
				c.Close()
			}
			return
		}
		fmt.Fprint(w, n.Load())
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}
