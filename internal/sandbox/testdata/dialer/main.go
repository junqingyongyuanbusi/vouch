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
		fmt.Fprintln(os.Stderr, "usage: dialer <host:port>")
		os.Exit(2)
	}
	conn, err := net.DialTimeout("tcp", os.Args[1], 3*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial failed:", err)
		os.Exit(1)
	}
	_ = conn.Close()
	fmt.Println("dial ok")
}
