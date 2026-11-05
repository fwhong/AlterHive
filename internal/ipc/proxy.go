package ipc

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type proxyListener struct{ net.Listener }

func ProxyListener(l net.Listener) net.Listener { return &proxyListener{l} }

type proxyConn struct {
	net.Conn
	r             *bufio.Reader
	remote, local net.Addr
}

func (c *proxyConn) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *proxyConn) RemoteAddr() net.Addr       { return c.remote }
func (c *proxyConn) LocalAddr() net.Addr        { return c.local }
func (l *proxyListener) Accept() (net.Conn, error) {
	for {
		c, e := l.Listener.Accept()
		if e != nil {
			return nil, e
		}
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		r := bufio.NewReaderSize(c, 256)
		line, e := r.ReadSlice('\n')
		parts := strings.Fields(string(line))
		if e != nil || len(parts) != 6 || parts[0] != "PROXY" || (parts[1] != "TCP4" && parts[1] != "TCP6") {
			c.Close()
			continue
		}
		src, dst := net.ParseIP(parts[2]), net.ParseIP(parts[3])
		sp, e1 := strconv.Atoi(parts[4])
		dp, e2 := strconv.Atoi(parts[5])
		if src == nil || dst == nil || e1 != nil || e2 != nil || sp < 0 || sp > 65535 || dp < 0 || dp > 65535 {
			c.Close()
			continue
		}
		c.SetReadDeadline(time.Time{})
		return &proxyConn{Conn: c, r: r, remote: &net.TCPAddr{IP: src, Port: sp}, local: &net.TCPAddr{IP: dst, Port: dp}}, nil
	}
}
func WriteProxyHeader(to net.Conn, from net.Conn) error {
	src, ok := from.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("TCP peer required")
	}
	dst, ok := from.LocalAddr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("TCP listener required")
	}
	family := "TCP4"
	if src.IP.To4() == nil {
		family = "TCP6"
	}
	_, e := fmt.Fprintf(to, "PROXY %s %s %s %d %d\r\n", family, src.IP, dst.IP, src.Port, dst.Port)
	return e
}
