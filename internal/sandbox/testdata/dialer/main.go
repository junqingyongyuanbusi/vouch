// Command dialer is a sandbox test helper: dials the address given as argv[1]
// and exits 0 on success, 1 on failure. It makes no assumptions about curl/nc.
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: dialer <host:port> | dialer selfloop")
		os.Exit(2)
	}
	if os.Args[1] == "selfloop" {
		// Listen and dial within the SAME process: this is how probes use
		// localhost (a test server and its client live inside one sandboxed
		// run). A Linux network namespace has its own loopback, so a listener
		// started outside the sandbox is unreachable by design.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, "listen failed:", err)
			os.Exit(1)
		}
		defer listener.Close()
		conn, derr := net.DialTimeout("tcp", listener.Addr().String(), 3*time.Second)
		if derr != nil {
			fmt.Fprintln(os.Stderr, "selfloop dial failed:", derr)
			os.Exit(1)
		}
		_ = conn.Close()
		fmt.Println("selfloop ok")
		return
	}
	conn, err := net.DialTimeout("tcp", os.Args[1], 3*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial failed:", err)
		os.Exit(1)
	}
	_ = conn.Close()
	fmt.Println("dial ok")
}
