package esi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// forwardedHeaders are the only request headers an include targeting another
// origin is sent with: the credentials the client has for this one are none
// of the other's business.
var forwardedHeaders = []string{"Accept", "Accept-Language", "User-Agent"}

// droppedHeaders are the request headers an include targeting the same origin
// is not sent with, as it asks for a whole fragment with a body-less GET.
var droppedHeaders = []string{
	// Conditional and range requests, whether from the client or from the
	// cache revalidating what it stored.
	"If-Match",
	"If-None-Match",
	"If-Modified-Since",
	"If-Unmodified-Since",
	"If-Range",
	"Range",
	// The request body.
	"Content-Encoding",
	"Content-Length",
	"Content-Type",
	"Expect",
	// The connection of the client.
	"Connection",
	"Keep-Alive",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
	// The included bytes have to be readable as they are: the encoding is
	// negotiated for each kind of fetch.
	"Accept-Encoding",
}

var (
	errAbandoned   = errors.New("the include is not waited for anymore")
	errRequestHost = errors.New("the host comes from the request, it is only served in process")
)

// fetch returns the URL ref points at, relative to base, and the body of the
// fragment found there, requested with depth as its depth in includes.
func (j *job) fetch(ref string, base *url.URL, depth int) (*url.URL, []byte, error) {
	if depth > maxDepth {
		return nil, nil, errTooDeep
	}

	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return nil, nil, err
	}

	target := base.ResolveReference(u)
	target.Fragment, target.RawFragment = "", ""
	if (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return nil, nil, fmt.Errorf("unsupported URL %s", target)
	}

	sameOrigin := target.Scheme == j.origin.Scheme && strings.EqualFold(target.Host, j.origin.Host)
	header := subrequestHeader(j.rq.Header, sameOrigin, depth)

	if sameOrigin && j.Local != nil {
		if handler := j.Local(j.rq); handler != nil {
			body, err := j.serveLocally(handler, target, header)

			return target, body, err
		}
	}

	// The host of a relative include is the one the client claims to reach:
	// the cache only connects to the hosts the templates name, or anyone
	// could have it fetch from anywhere.
	if u.Host == "" && strings.EqualFold(target.Host, j.origin.Host) {
		return nil, nil, errRequestHost
	}

	body, err := j.fetchRemotely(target, header)

	return target, body, err
}

// subrequestHeader returns the header to request an include with, out of the
// one of the request being served.
func subrequestHeader(parent http.Header, sameOrigin bool, depth int) http.Header {
	header := http.Header{}

	if sameOrigin {
		// The fragment is requested on behalf of the client, as the client
		// would have: it gets the same variant and the same credentials.
		if cloned := parent.Clone(); cloned != nil {
			header = cloned
		}

		for _, name := range droppedHeaders {
			header.Del(name)
		}
	} else {
		for _, name := range forwardedHeaders {
			if values := parent.Values(name); len(values) > 0 {
				header[name] = slices.Clone(values)
			}
		}
	}

	header.Set(DepthHeader, strconv.Itoa(depth))

	return header
}

// serveLocally has handler serve the include for target as if the client had
// requested it on the connection of the request being served.
func (j *job) serveLocally(handler http.Handler, target *url.URL, header http.Header) ([]byte, error) {
	rq, err := http.NewRequestWithContext(j.rq.Context(), http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return nil, err
	}

	// A server gets the target of a request as a path, the host apart.
	rq.URL.Scheme, rq.URL.Host = "", ""
	rq.RequestURI = rq.URL.RequestURI()
	rq.RemoteAddr = j.rq.RemoteAddr
	rq.TLS = j.rq.TLS
	rq.Header = header
	rq.Header.Set("Accept-Encoding", "identity")

	rec := &recorder{header: http.Header{}}
	handler.ServeHTTP(rec, rq)

	return rec.result()
}

// fetchRemotely requests the include for target over the network.
func (j *job) fetchRemotely(target *url.URL, header http.Header) ([]byte, error) {
	rq, err := http.NewRequestWithContext(j.rq.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}

	// Without an Accept-Encoding of its own, the request gets a compressed
	// response the transport decompresses.
	rq.Header = header

	client := j.Client
	if client == nil {
		client = http.DefaultClient
	}

	res, err := client.Do(rq)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = res.Body.Close()
	}()

	if err := checkFragment(res.StatusCode, res.Header); err != nil {
		return nil, err
	}

	return io.ReadAll(res.Body)
}

// checkFragment tells whether a response is a fragment that can be included.
func checkFragment(status int, header http.Header) error {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return fmt.Errorf("unexpected status %d", status)
	}

	if encoding := header.Get("Content-Encoding"); !isIdentity(encoding) {
		return fmt.Errorf("unexpected %s encoded body", encoding)
	}

	return nil
}

// recorder collects the response a handler gives to an include served in
// process.
type recorder struct {
	mu        sync.Mutex
	header    http.Header
	body      bytes.Buffer
	status    int
	abandoned bool
}

func (r *recorder) Header() http.Header {
	return r.header
}

func (r *recorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Interim responses tell nothing about the fragment.
	if r.status == 0 && code >= http.StatusOK {
		r.status = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.abandoned {
		return 0, errAbandoned
	}

	if r.status == 0 {
		r.status = http.StatusOK
	}

	return r.body.Write(b)
}

// result returns the fragment the handler answered with. A handler still
// running in the background can't write to it anymore from then on.
func (r *recorder) result() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.abandoned = true

	status := r.status
	if status == 0 {
		status = http.StatusOK
	}

	if err := checkFragment(status, r.header); err != nil {
		return nil, err
	}

	return r.body.Bytes(), nil
}
