package main

import (
	"fmt"
	"io"
	"net/http"

	incus "github.com/lxc/incus/v7/client"
)

// readOnlyTransport only lets GET requests through.
type readOnlyTransport struct {
	base http.RoundTripper
	log  io.Writer
}

// RoundTrip forwards GET requests and fails anything else without sending it.
func (t *readOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != "" {
		if req.Body != nil {
			_ = req.Body.Close()
		}

		return nil, fmt.Errorf("Refusing to send %s %s, only GET requests are allowed", req.Method, req.URL.Path)
	}

	if t.log != nil {
		_, _ = fmt.Fprintf(t.log, "GET %s\n", req.URL.RequestURI())
	}

	return t.base.RoundTrip(req)
}

// restrictToReadOnly makes all further requests on the connection go through a readOnlyTransport.
func restrictToReadOnly(server incus.InstanceServer, log io.Writer) error {
	client, err := server.GetHTTPClient()
	if err != nil {
		return err
	}

	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}

	client.Transport = &readOnlyTransport{base: base, log: log}

	return nil
}
