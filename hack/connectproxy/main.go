// Command connectproxy is the HTTP proxy of hack/qemu-acme-test.sh run with
// QEMU_ACME_VIA_PROXY=1: CONNECT tunnels and plain-HTTP forwarding, Basic
// credentials required when -user is set, one line logged per request,
// and -map rewriting a host the guest names (QEMU's 10.0.2.2) to the one
// this host reaches it by. With -stdio it serves one connection on its
// standard input and output - what a QEMU guestfwd's cmd: gives it, one
// process per connection the guest opens (a guestfwd to a tcp: chardev
// carries a single connection).
//
//	go run ./hack/connectproxy -listen 127.0.0.1:3128 -user acme -password s3cret -map 10.0.2.2=127.0.0.1
//	guestfwd=tcp:10.0.2.100:3128-cmd:connectproxy -stdio -log proxy.log -user acme -password s3cret
package main

import (
	"bufio"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:3128", "where to listen")
	stdio := flag.Bool("stdio", false, "serve one connection on standard input and output")
	logFile := flag.String("log", "", "append the log to this file (with -stdio, stdout is the connection)")
	user := flag.String("user", "", "the username asked for (none: no credentials)")
	password := flag.String("password", "", "its password")
	mapping := flag.String("map", "", "FROM=TO: connect to TO when asked for FROM")
	flag.Parse()
	log.SetFlags(log.Ltime)
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		log.SetOutput(f)
	}
	from, to, _ := strings.Cut(*mapping, "=")
	p := &proxy{user: *user, password: *password, from: from, to: to}
	if *stdio {
		p.serveOne(bufio.NewReader(os.Stdin), os.Stdout)
		return
	}
	srv := &http.Server{Addr: *listen, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("listening on %s", *listen)
	log.Fatal(srv.ListenAndServe())
}

type proxy struct{ user, password, from, to string }

func (p *proxy) mapHost(hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err == nil && p.from != "" && host == p.from {
		return net.JoinHostPort(p.to, port)
	}
	return hostport
}

// authorized says r carries the credentials asked for, logging a refusal.
func (p *proxy) authorized(r *http.Request) bool {
	if p.user == "" {
		return true
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.password))
	if r.Header.Get("Proxy-Authorization") == want {
		return true
	}
	log.Printf("refused %s %s: credentials", r.Method, r.Host)
	return false
}

// forward sends a plain-HTTP request on to its host.
func (p *proxy) forward(r *http.Request) (*http.Response, error) {
	log.Printf("%s %s", r.Method, r.URL)
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Host = p.mapHost(r.URL.Host)
	out.Header.Del("Proxy-Authorization")
	return (&http.Transport{}).RoundTrip(out)
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="test"`)
		http.Error(w, "proxy credentials", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		log.Printf("CONNECT %s", r.Host)
		up, err := net.DialTimeout("tcp", p.mapHost(r.Host), 10*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// Hijacked before answering, and read through its buffer: the
		// server may read the client's first bytes ahead once the answer
		// is out.
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			up.Close()
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			up.Close()
			conn.Close()
			return
		}
		go func() { _, _ = io.Copy(up, buf); up.Close() }()
		_, _ = io.Copy(conn, up)
		conn.Close()
		return
	}
	res, err := p.forward(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	for k, v := range res.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
}

// serveOne serves the requests of one connection - in, out - until it
// closes or becomes a tunnel.
func (p *proxy) serveOne(in *bufio.Reader, out io.Writer) {
	status := func(code int, extra string) {
		fmt.Fprintf(out, "HTTP/1.1 %d %s\r\n%sContent-Length: 0\r\n\r\n", code, http.StatusText(code), extra)
	}
	for {
		r, err := http.ReadRequest(in)
		if err != nil {
			return
		}
		if !p.authorized(r) {
			status(http.StatusProxyAuthRequired, "Proxy-Authenticate: Basic realm=\"test\"\r\n")
			continue
		}
		if r.Method == http.MethodConnect {
			log.Printf("CONNECT %s", r.Host)
			up, err := net.DialTimeout("tcp", p.mapHost(r.Host), 10*time.Second)
			if err != nil {
				status(http.StatusBadGateway, "")
				return
			}
			fmt.Fprint(out, "HTTP/1.1 200 Connection established\r\n\r\n")
			go func() {
				_, _ = io.Copy(up, in) // what was read ahead included
				if tc, ok := up.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
			}()
			_, _ = io.Copy(out, up)
			up.Close()
			return
		}
		res, err := p.forward(r)
		if err != nil {
			status(http.StatusBadGateway, "")
			return
		}
		err = res.Write(out)
		res.Body.Close()
		if err != nil {
			return
		}
	}
}
