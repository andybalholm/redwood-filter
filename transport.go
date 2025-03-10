package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"
	"syscall"
	"time"

	ftp "github.com/remogatto/ftpget"
	"golang.org/x/net/http2"
)

var dialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
	DualStack: true,
}

var httpTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           dialer.DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	DisableKeepAlives:     true,
}

var http2Transport = &http2.Transport{}

func dialWithExtraRootCerts(network, addr string) (net.Conn, error) {
	// Dial a TLS connection, and make sure it is valid against either the system default
	// roots or conf.ExtraRootCerts.
	serverName, _, _ := net.SplitHostPort(addr)
	conn, err := tls.DialWithDialer(dialer, network, addr, &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true,
	})
	if err != nil {
		return nil, err
	}
	state := conn.ConnectionState()
	serverCert := state.PeerCertificates[0]

	chains, err := serverCert.Verify(x509.VerifyOptions{
		Intermediates: certPoolWith(state.PeerCertificates[1:]),
		DNSName:       serverName,
	})
	if err == nil {
		state.VerifiedChains = chains
		return conn, nil
	}

	if conf := getConfig(); conf.ExtraRootCerts != nil {
		chains, err = serverCert.Verify(x509.VerifyOptions{
			Intermediates: certPoolWith(state.PeerCertificates[1:]),
			DNSName:       serverName,
			Roots:         conf.ExtraRootCerts,
		})
		if err == nil {
			state.VerifiedChains = chains
			return conn, nil
		}
	}

	conn.Close()
	return nil, err
}

var transportWithExtraRootCerts = &http.Transport{
	DialTLS:               dialWithExtraRootCerts,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

var clientWithExtraRootCerts = &http.Client{
	Transport: transportWithExtraRootCerts,
}

// A connTransport is an http.RoundTripper that uses a single network
// connection.
type connTransport struct {
	Conn   net.Conn
	Redial func(context.Context) (net.Conn, error)

	br   *bufio.Reader
	used bool
}

func (ct *connTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if ct.used && !requestIsReplayable(req) {
		// If the request is not replayable, make sure we have a new connection,
		// not a reused one.
		if redialErr := ct.redial(req.Context()); err != nil {
			logVerbose("redial", "Error redialing connection to %s: %v", req.Host, redialErr)
		}
	}
	ct.used = true

	resp, err = ct.roundTrip(req)

	if err != nil && shouldRedialForError(err) && requestIsReplayable(req) {
		// Retry with a new network connection.
		if redialErr := ct.redial(req.Context()); redialErr == nil {
			resp, err = ct.roundTrip(req)
		} else {
			logVerbose("redial", "Error redialing connection to %s: %v", req.Host, redialErr)
		}
	}

	return
}

func shouldRedialForError(err error) bool {
	switch {
	case errors.Is(err, io.EOF):
		return true
	case errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case errors.Is(err, syscall.EPIPE):
		return true
	case strings.Contains(err.Error(), "no renegotiation"):
		return true
	default:
		return false
	}
}

// This is copied from http.Request.isReplayable, with a few changes to
// eliminate unexported functions.
func requestIsReplayable(r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody || r.GetBody != nil {
		switch r.Method {
		case "GET", "HEAD", "OPTIONS", "TRACE":
			return true
		}
		// The Idempotency-Key, while non-standard, is widely used to
		// mean a POST or other request is idempotent. See
		// https://golang.org/issue/19943#issuecomment-421092421
		if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "" {
			return true
		}
	}
	return false
}

func (ct *connTransport) roundTrip(req *http.Request) (resp *http.Response, err error) {
	ctx := req.Context()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		// Continue.
	}

	if err := req.Write(ct.Conn); err != nil {
		return nil, err
	}

	if ct.br == nil {
		ct.br = bufio.NewReader(ct.Conn)
	}

	resp, err = ReadResponse(ct.br, req)
	if err == nil {
		resp.Body = &bodyWithContext{
			ReadCloser: resp.Body,
			Ctx:        ctx,
		}
	}
	return resp, err
}

func (ct *connTransport) redial(ctx context.Context) error {
	if ct.Redial == nil {
		return errors.New("no redial function provided")
	}
	newConn, err := ct.Redial(ctx)
	if err != nil {
		return err
	}
	ct.Conn.Close()
	ct.Conn = newConn
	ct.br = bufio.NewReader(ct.Conn)
	return nil
}

// A bodyWithContext wraps a response body, and makes Read return an error if
// the associated context is canceled.
type bodyWithContext struct {
	io.ReadCloser
	Ctx context.Context
}

func (b *bodyWithContext) Read(p []byte) (n int, err error) {
	select {
	case <-b.Ctx.Done():
		return 0, b.Ctx.Err()
	default:
		return b.ReadCloser.Read(p)
	}
}

// An FTPTransport fetches files via FTP.
type FTPTransport struct{}

func (FTPTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if req.Method != "GET" {
		return &http.Response{
			StatusCode: http.StatusMethodNotAllowed,
			Request:    req,
		}, nil
	}

	fullPath := req.URL.Host + req.URL.Path
	r, w := io.Pipe()
	xfer, err := ftp.GetAsync(fullPath, w)
	if err != nil {
		return nil, err
	}

	go func() {
		for stat := range xfer.Status {
			switch stat {
			case ftp.COMPLETED:
				w.Close()
				return
			case ftp.ERROR:
				err := <-xfer.Error
				log.Printf("FTP: error downloading %v: %v", req.URL, err)
				w.CloseWithError(err)
				return
			}
		}
	}()

	resp = &http.Response{
		StatusCode: 200,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Request:    req,
		Body:       r,
		Header:     make(http.Header),
	}

	ext := path.Ext(req.URL.Path)
	if ext != "" {
		ct := mime.TypeByExtension(ext)
		if ct != "" {
			resp.Header.Set("Content-Type", ct)
		}
	}

	return resp, nil
}

// A RetryTransport wraps an http.RoundTripper to automatically retry
// failed requests.
type RetryTransport struct {
	transport http.RoundTripper
}

func (t *RetryTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if requestIsReplayable(req) {
		for range 3 {
			resp, err = t.transport.RoundTrip(req)
			if err == nil || !shouldRedialForError(err) {
				return resp, err
			}
			logVerbose("redial", "retrying request for %v", req.URL)
		}
	}
	return t.transport.RoundTrip(req)
}
