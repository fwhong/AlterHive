package main

import (
	"crypto/subtle"
	"github.com/alterhive/alterhive/api"
	"github.com/alterhive/alterhive/internal/ipc"
	"github.com/alterhive/alterhive/internal/sandbox"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// ingress holds only a session-admin credential, never provider/manager keys.
// It exposes TCP; the sensor itself has network_mode:none and only Unix sockets.
func runIngress() {
	requireAdminToken()
	sshSocket := getEnv("ALTERHIVE_SSH_SOCKET", "/run/alterhive/ingress/ssh.sock")
	apiSocket := getEnv("ALTERHIVE_API_SOCKET", "/run/alterhive/ingress/api.sock")
	sshListener, err := net.Listen("tcp", getEnv("SSH_ADDR", ":2222"))
	if err != nil {
		log.Fatal(err)
	}
	defer sshListener.Close()
	target, _ := url.Parse("http://sensor")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = ipc.Client(apiSocket).Transport
	original := proxy.Director
	proxy.Director = func(r *http.Request) { original(r); r.Header.Del("Authorization"); r.Header.Del("Cookie") }
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) { http.Error(w, "sensor unavailable", 503) }
	gin.SetMode(gin.ReleaseMode)
	auth := gin.New()
	auth.Use(gin.Recovery(), corsMiddleware())
	api.NewServer(nil, nil).RegisterAuthRoutes(auth.Group("/api/v1"))
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", auth)
	mux.Handle("/healthz", proxy)
	token := os.Getenv("ALTERHIVE_ADMIN_TOKEN")
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		// Never deliver LLM credentials or administration to the untrusted sensor.
		if strings.HasPrefix(r.URL.Path, "/api/v1/llm/") {
			http.NotFound(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		proxy.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", getEnv("API_ADDR", ":8000"))
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	if err := sandbox.LockNetwork(); err != nil {
		log.Fatalf("cannot lock ingress egress: %v", err)
	}
	go proxySSH(sshListener, sshSocket)
	serveHTTP(ln, mux)
	log.Info("ingress started: TCP to sensor Unix sockets; ops credential stripped upstream")
	waitForShutdown()
}
func proxySSH(l net.Listener, path string) {
	slots := make(chan struct{}, 128)
	for {
		c, e := l.Accept()
		if e != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			defer c.Close()
			upstream, e := net.DialTimeout("unix", path, 3*time.Second)
			if e != nil {
				return
			}
			defer upstream.Close()
			c.SetDeadline(time.Now().Add(10 * time.Minute))
			upstream.SetDeadline(time.Now().Add(10 * time.Minute))
			if ipc.WriteProxyHeader(upstream, c) != nil {
				return
			}
			done := make(chan struct{}, 1)
			go func() { io.Copy(upstream, c); done <- struct{}{} }()
			io.Copy(c, upstream)
			c.Close()
			upstream.Close()
			<-done
		}()
	}
}
