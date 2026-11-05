package ipc

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func socketPath(t *testing.T) string {
	t.Helper()
	d, e := os.MkdirTemp("", "ah-ipc-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "rpc.sock")
}
func TestUIDAuthenticatedUnixListener(t *testing.T) {
	p := socketPath(t)
	l, e := Listen(p, uint32(os.Getuid()))
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })}
	go s.Serve(l)
	defer s.Close()
	c := Client(p)
	defer c.CloseIdleConnections()
	resp, e := c.Get("http://unused/healthz")
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("request failed")
	}
	if _, e := Listen(p, uint32(os.Getuid())); e == nil {
		t.Fatal("replaced live socket")
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0660 {
		t.Fatal("socket mode")
	}
}
func TestWrongUIDRejectedAndNonSocketPreserved(t *testing.T) {
	p := socketPath(t)
	l, e := Listen(p, uint32(os.Getuid()+1))
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unauthorized peer reached handler") })}
	go s.Serve(l)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://unused/", nil)
	c := Client(p)
	defer c.CloseIdleConnections()
	if resp, e := c.Do(req); e == nil {
		resp.Body.Close()
		t.Fatal("wrong UID accepted")
	}
	p = socketPath(t)
	os.WriteFile(p, []byte("keep"), 0600)
	if _, e := Listen(p, uint32(os.Getuid())); e == nil {
		t.Fatal("replaced regular file")
	}
	data, _ := os.ReadFile(p)
	if string(data) != "keep" {
		t.Fatal("destroyed file")
	}
}
