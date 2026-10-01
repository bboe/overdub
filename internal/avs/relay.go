package avs

import (
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"

	"github.com/bboe/overdub/internal/untrustedlog"
)

const (
	Endpoint = "avs-alexa-4-na.amazon.com:443"

	idleTimeout = 30 * time.Second
	pingTimeout = 15 * time.Second

	acceptPauseMin = 5 * time.Millisecond
	acceptPauseMax = time.Second
)

type Relay struct {
	addr   string
	target string
	cert   tls.Certificate

	log untrustedlog.Log

	tr *http2.Transport

	reqN atomic.Int64
}

func New(addr string, cert tls.Certificate) *Relay {
	return &Relay{
		addr:   addr,
		target: Endpoint,
		cert:   cert,
		log:    untrustedlog.Log{Subject: "avs"},
		tr:     &http2.Transport{ReadIdleTimeout: idleTimeout, PingTimeout: pingTimeout},
	}
}

func (r *Relay) Listen() (net.Listener, error) {
	return tls.Listen("tcp", r.addr, &tls.Config{
		Certificates: []tls.Certificate{r.cert},
		NextProtos:   []string{"h2"},
		MinVersion:   tls.VersionTLS12,
	})
}

func (r *Relay) Serve(ln net.Listener) error {
	srv := &http2.Server{}
	h := http.HandlerFunc(r.forward)
	var pause time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			if pause = min(2*pause, acceptPauseMax); pause == 0 {
				pause = acceptPauseMin
			}
			log.Printf("avs: accept failed, waiting %v: %v", pause, err)
			time.Sleep(pause)
			continue
		}
		pause = 0
		go func() {
			defer conn.Close()
			tc, ok := conn.(*tls.Conn)
			if !ok {
				return
			}
			if err := tc.Handshake(); err != nil {
				r.log.Printf("handshake from %s failed: %s",
					conn.RemoteAddr(), untrustedlog.Cut(err.Error()))
				return
			}
			srv.ServeConn(conn, &http2.ServeConnOpts{Handler: h})
		}()
	}
}

func (r *Relay) forward(w http.ResponseWriter, req *http.Request) {
	id := r.reqN.Add(1)
	host, _, _ := net.SplitHostPort(r.target)
	out, err := http.NewRequestWithContext(req.Context(), req.Method,
		"https://"+r.target+req.URL.RequestURI(), req.Body)
	if err != nil {
		r.log.Printf("req %d: %s", id, untrustedlog.Cut(err.Error()))
		panic(http.ErrAbortHandler)
	}
	out.Header = req.Header.Clone()
	out.Host = host
	out.ContentLength = req.ContentLength

	resp, err := r.tr.RoundTrip(out)
	if err != nil {
		r.log.Printf("req %d: %s failed: %s", id,
			untrustedlog.Cut(req.URL.Path), untrustedlog.Cut(err.Error()))
		panic(http.ErrAbortHandler)
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if err := copyFlush(w, resp.Body); err != nil {
		r.log.Printf("req %d: relaying the response: %s", id, untrustedlog.Cut(err.Error()))
	}
}

func copyFlush(w http.ResponseWriter, src io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
