package middleware

import (
	"bytes"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkweak/souin/pkg/esi"
	"github.com/darkweak/souin/pkg/rfc"
)

type SouinWriterInterface interface {
	http.ResponseWriter
	Send() (int, error)
}

var _ SouinWriterInterface = (*CustomWriter)(nil)

func NewCustomWriter(rq *http.Request, rw http.ResponseWriter, b *bytes.Buffer) *CustomWriter {
	return &CustomWriter{
		statusCode: 200,
		Buf:        b,
		Req:        rq,
		Rw:         rw,
		Headers:    http.Header{},
		mutex:      sync.Mutex{},
	}
}

// CustomWriter handles the response and provide the way to cache the value
type CustomWriter struct {
	Buf         *bytes.Buffer
	Rw          http.ResponseWriter
	Req         *http.Request
	Headers     http.Header
	ESI         *esi.Processor
	mutex       sync.Mutex
	statusCode  int
	headersSent atomic.Bool
}

func (r *CustomWriter) handleBuffer(callback func(*bytes.Buffer)) {
	r.mutex.Lock()
	callback(r.Buf)
	r.mutex.Unlock()
}

// Header will write the response headers
func (r *CustomWriter) Header() http.Header {
	if r.headersSent.Load() || r.Req.Context().Err() != nil {
		return http.Header{}
	}

	return r.Rw.Header()
}

// GetStatusCode returns the response status code
func (r *CustomWriter) GetStatusCode() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return r.statusCode
}

// WriteHeader will write the response headers
func (r *CustomWriter) WriteHeader(code int) {
	if r.headersSent.Load() {
		return
	}

	// An interim response belongs to the exchange with the client, not to the
	// representation being cached (RFC 9111 section 3): forward it as it
	// comes and keep waiting for the final status.
	if code >= http.StatusContinue && code < http.StatusOK {
		r.Rw.WriteHeader(code)

		return
	}

	r.mutex.Lock()
	r.statusCode = code
	r.mutex.Unlock()
}

// Write will write the response body
func (r *CustomWriter) Write(b []byte) (int, error) {
	r.handleBuffer(func(actual *bytes.Buffer) {
		actual.Grow(len(b))
		_, _ = actual.Write(b)
	})

	return len(b), nil
}

// Send delays the response to handle Cache-Status
func (r *CustomWriter) Send() (int, error) {
	defer r.handleBuffer(func(b *bytes.Buffer) {
		b.Reset()
	})

	// Header stops handing out the response headers once the request context
	// is done, which resolving the ESI includes may outlast: stick to the ones
	// Send started with.
	header := r.Header()

	storedLength := header.Get(rfc.StoredLengthHeader)
	if storedLength != "" {
		header.Set("Content-Length", storedLength)
	}

	r.mutex.Lock()
	result := r.Buf.Bytes()
	r.mutex.Unlock()

	// The stored body keeps its ESI tags, which are resolved every time it is
	// served, as long as the headers can still be updated to describe the
	// assembled body, and the body is not a mere part of the stored one.
	if r.ESI != nil && !r.headersSent.Load() && r.Req.Context().Err() == nil && r.GetStatusCode() != http.StatusPartialContent {
		result = r.ESI.Process(r.Req, header, result)
	}

	header.Del(rfc.StoredLengthHeader)
	header.Del(rfc.StoredTTLHeader)
	header.Del(rfc.StoredExpiryHeader)

	// When the client issued a range request, serve it from the fully cached
	// body through the standard library. http.ServeContent implements RFC 7233
	// in full (single/suffix/multipart ranges, If-Range, 416 with Content-Range,
	// Accept-Ranges and CRLF-delimited multipart payloads), which avoids the
	// off-by-one and out-of-bounds issues of a hand-rolled implementation.
	if rangeHeader := r.Headers.Get("Range"); rangeHeader != "" && r.GetStatusCode() == http.StatusOK && !r.headersSent.Load() {
		header.Set("Accept-Ranges", "bytes")

		// ServeContent reads Range/If-Range from the request. Build a minimal
		// request carrying only those headers so it doesn't re-evaluate the
		// conditional headers Souin already handled upstream.
		rangeReq := &http.Request{Method: r.Req.Method, Header: http.Header{"Range": {rangeHeader}}}
		if ifRange := r.Req.Header.Get("If-Range"); ifRange != "" {
			rangeReq.Header.Set("If-Range", ifRange)
		}

		// Last-Modified lets ServeContent evaluate a date-based If-Range.
		var modtime time.Time
		if lastModified := header.Get("Last-Modified"); lastModified != "" {
			if parsed, err := http.ParseTime(lastModified); err == nil {
				modtime = parsed
			}
		}

		r.headersSent.Swap(true)
		// An empty name skips extension-based sniffing so the cached
		// Content-Type is preserved.
		http.ServeContent(r.Rw, rangeReq, "", modtime, bytes.NewReader(result))

		return len(result), nil
	}

	if len(result) != 0 {
		header.Set("Content-Length", strconv.Itoa(len(result)))
	}

	header.Del(rfc.StoredLengthHeader)
	header.Del(rfc.StoredTTLHeader)
	header.Del(rfc.StoredExpiryHeader)

	if !r.headersSent.Load() {
		r.mutex.Lock()
		r.Rw.WriteHeader(r.statusCode)
		r.mutex.Unlock()

		r.headersSent.Store(true)
	}

	return r.Rw.Write(result)
}
