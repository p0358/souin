package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/darkweak/souin/pkg/esi"
	"github.com/darkweak/souin/pkg/rfc"
)

// sendWithRange builds a CustomWriter holding the given body, simulates the
// stashed Range header (see ServeHTTP) and returns the result of Send().
func sendWithRange(t *testing.T, body, rangeHeader string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/segment.ts", nil)
	cw := NewCustomWriter(req, rec, &bytes.Buffer{})
	rec.Header().Set("Content-Type", "video/mp2t")
	if rangeHeader != "" {
		cw.Headers.Set("Range", rangeHeader)
	}
	_, _ = cw.Write([]byte(body))
	if _, err := cw.Send(); err != nil {
		t.Fatalf("Send returned an error: %v", err)
	}

	return rec
}

func TestSend_NoRangeServesFullBody(t *testing.T) {
	rec := sendWithRange(t, "0123456789", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "0123456789" {
		t.Fatalf("expected full body, got %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Range") != "" {
		t.Fatalf("unexpected Content-Range on a non-range response")
	}
}

func TestSend_SingleRange(t *testing.T) {
	rec := sendWithRange(t, "0123456789", "bytes=0-3")

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "0123" {
		t.Fatalf("expected %q, got %q", "0123", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-3/10" {
		t.Fatalf("expected Content-Range %q, got %q", "bytes 0-3/10", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("expected Accept-Ranges bytes, got %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "video/mp2t" {
		t.Fatalf("cached Content-Type should be preserved, got %q", got)
	}
}

func TestSend_OpenEndedRange(t *testing.T) {
	rec := sendWithRange(t, "0123456789", "bytes=5-")

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "56789" {
		t.Fatalf("expected %q, got %q", "56789", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 5-9/10" {
		t.Fatalf("expected Content-Range %q, got %q", "bytes 5-9/10", got)
	}
}

func TestSend_SuffixRange(t *testing.T) {
	rec := sendWithRange(t, "0123456789", "bytes=-3")

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "789" {
		t.Fatalf("expected %q, got %q", "789", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 7-9/10" {
		t.Fatalf("expected Content-Range %q, got %q", "bytes 7-9/10", got)
	}
}

func TestSend_UnsatisfiableRange(t *testing.T) {
	rec := sendWithRange(t, "0123456789", "bytes=100-200")

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */10" {
		t.Fatalf("expected Content-Range %q, got %q", "bytes */10", got)
	}
}

func TestSend_MultipartRange(t *testing.T) {
	rec := sendWithRange(t, "0123456789", "bytes=0-1, 4-5")

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" ||
		!bytes.Contains([]byte(ct), []byte("multipart/byteranges")) {
		t.Fatalf("expected multipart/byteranges Content-Type, got %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"bytes 0-1/10", "bytes 4-5/10", "01", "45"} {
		if !bytes.Contains([]byte(body), []byte(want)) {
			t.Fatalf("multipart body missing %q; got:\n%s", want, body)
		}
	}
}

// sendWithESI builds a CustomWriter holding the given body as it would come
// out of the cache, resolves its ESI includes against fragments and returns
// the result of Send().
func sendWithESI(t *testing.T, body string, statusCode int, rangeHeader string) *httptest.ResponseRecorder {
	t.Helper()

	fragments := http.NewServeMux()
	fragments.HandleFunc("/large", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("0123456789"), 10000))
	})
	fragments.HandleFunc("/name", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("World"))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)
	cw := NewCustomWriter(req, rec, &bytes.Buffer{})
	cw.ESI = &esi.Processor{Local: func(*http.Request) http.Handler { return fragments }}
	rec.Header().Set("Content-Type", "text/html")
	rec.Header().Set("Content-Length", strconv.Itoa(len(body)))
	rec.Header().Set(rfc.StoredLengthHeader, strconv.Itoa(len(body)))
	rec.Header().Set("Etag", `"template"`)
	if rangeHeader != "" {
		cw.Headers.Set("Range", rangeHeader)
	}
	cw.WriteHeader(statusCode)
	_, _ = cw.Write([]byte(body))
	if _, err := cw.Send(); err != nil {
		t.Fatalf("Send returned an error: %v", err)
	}

	return rec
}

func TestSend_ESIGrowsTheBody(t *testing.T) {
	rec := sendWithESI(t, `<p><esi:include src="/large"/></p>`, http.StatusOK, "")

	expected := "<p>" + strings.Repeat("0123456789", 10000) + "</p>"
	if rec.Code != http.StatusOK || rec.Body.String() != expected {
		t.Fatalf("expected the whole assembled body, got %d bytes with status %d", rec.Body.Len(), rec.Code)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(expected)) {
		t.Fatalf("expected the Content-Length of the assembled body %d, got %s", len(expected), got)
	}
	if rec.Header().Get(rfc.StoredLengthHeader) != "" || rec.Header().Get("Etag") != "" {
		t.Fatalf("unexpected headers %v", rec.Header())
	}
}

func TestSend_ESIEmptiesTheBody(t *testing.T) {
	rec := sendWithESI(t, `<esi:include src="/missing" onerror="continue"/>`, http.StatusOK, "")

	if rec.Body.Len() != 0 {
		t.Fatalf("expected an empty body, got %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != "0" {
		t.Fatalf("the stored length should not be announced for an empty body, got %s", got)
	}
}

func TestSend_ESIRange(t *testing.T) {
	rec := sendWithESI(t, `Hello <esi:include src="/name"/>!`, http.StatusOK, "bytes=6-")

	if rec.Code != http.StatusPartialContent || rec.Body.String() != "World!" {
		t.Fatalf("expected a range of the assembled body, got %q with status %d", rec.Body.String(), rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 6-11/12" {
		t.Fatalf("expected Content-Range %q, got %q", "bytes 6-11/12", got)
	}
}

func TestSend_ESISkipsPartialContent(t *testing.T) {
	body := `<esi:include src="/name"/>`
	rec := sendWithESI(t, body, http.StatusPartialContent, "")

	if rec.Body.String() != body {
		t.Fatalf("a part of a body should be left as is, got %q", rec.Body.String())
	}
}
