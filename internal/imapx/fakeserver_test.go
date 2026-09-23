package imapx

import (
	"bufio"
	"net"
	"strings"
	"testing"

	"github.com/lhns/umleitung/internal/config"
)

// scriptedServer is a minimal line-based IMAP server for protocol edge cases
// the in-memory server can't produce (BAD responses, unanswered commands).
// handle gets the tag and the command line after the tag and returns the
// lines to send; nil sends nothing. LOGIN and CAPABILITY are pre-handled.
func scriptedServer(t *testing.T, handle func(tag, cmd string) []string) config.Endpoint {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := make(chan net.Conn, 1)
	t.Cleanup(func() {
		ln.Close()
		select {
		case c := <-conns:
			c.Close()
		default:
		}
	})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conns <- conn
		w := func(lines ...string) {
			for _, l := range lines {
				if _, err := conn.Write([]byte(l + "\r\n")); err != nil {
					return
				}
			}
		}
		w("* OK [CAPABILITY IMAP4rev1] ready")
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			tag, cmd, _ := strings.Cut(sc.Text(), " ")
			switch verb, _, _ := strings.Cut(cmd, " "); strings.ToUpper(verb) {
			case "LOGIN":
				w(tag + " OK LOGIN completed")
			case "CAPABILITY":
				w("* CAPABILITY IMAP4rev1", tag+" OK CAPABILITY completed")
			default:
				w(handle(tag, cmd)...)
			}
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	return config.Endpoint{Host: "127.0.0.1", Port: port, User: "u", Password: "p", Folder: "INBOX"}
}
