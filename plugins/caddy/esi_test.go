package httpcache

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
)

func assertCacheHit(t *testing.T, resp *http.Response) {
	t.Helper()

	if cacheStatus := resp.Header.Get("Cache-Status"); !strings.HasPrefix(cacheStatus, "Souin; hit;") {
		t.Errorf("expected a cache hit, got %q", cacheStatus)
	}
}

func assertContentLength(t *testing.T, resp *http.Response, body string) {
	t.Helper()

	if resp.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Errorf("expected a Content-Length of %d, got %q", len(body), resp.Header.Get("Content-Length"))
	}
}

func TestESIFragmentsAreCachedOnTheirOwn(t *testing.T) {
	var pageHits, headerHits, footerHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/esi-fragments/page":
			pageHits.Add(1)
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("Etag", `"page"`)
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
			_, _ = w.Write([]byte(`<esi:include src="header"/><main>content</main><esi:include src="/esi-fragments/footer"/>`))
		case "/esi-fragments/header":
			// The header changes often, it is stored for a second only.
			w.Header().Set("Cache-Control", "max-age=1")
			_, _ = fmt.Fprintf(w, "<header>v%d</header>", headerHits.Add(1))
		case "/esi-fragments/footer":
			w.Header().Set("Cache-Control", "max-age=60")
			_, _ = fmt.Fprintf(w, "<footer>v%d</footer>", footerHits.Add(1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	tester := caddytest.NewTester(t)
	tester.InitServer(fmt.Sprintf(`
	{
		admin localhost:2999
		http_port     9080
		https_port    9443
		cache {
			esi
		}
	}
	localhost:9080 {
		route /esi-fragments/* {
			cache
			reverse_proxy %s
		}
	}`, backend.Listener.Addr()), "caddyfile")

	assertBackendHits := func(page, header, footer int32) {
		t.Helper()

		if pageHits.Load() != page || headerHits.Load() != header || footerHits.Load() != footer {
			t.Errorf("expected %d/%d/%d backend hits for the page/header/footer, got %d/%d/%d",
				page, header, footer, pageHits.Load(), headerHits.Load(), footerHits.Load())
		}
	}

	expected := "<header>v1</header><main>content</main><footer>v1</footer>"
	resp1, _ := tester.AssertGetResponse(`http://localhost:9080/esi-fragments/page`, 200, expected)
	if resp1.Header.Get("Cache-Status") != "Souin; fwd=uri-miss; stored; key=GET-http-localhost:9080-/esi-fragments/page" {
		t.Errorf("unexpected Cache-Status header %v", resp1.Header.Get("Cache-Status"))
	}
	assertContentLength(t, resp1, expected)
	// The validators of the stored template don't identify the assembled page,
	// which would let a client revalidate a page with outdated fragments.
	if resp1.Header.Get("Etag") != "" || resp1.Header.Get("Last-Modified") != "" {
		t.Errorf("unexpected validators %v", resp1.Header)
	}
	assertBackendHits(1, 1, 1)

	// Everything is cached now: the page is assembled without reaching the
	// backend.
	resp2, _ := tester.AssertGetResponse(`http://localhost:9080/esi-fragments/page`, 200, expected)
	assertCacheHit(t, resp2)
	assertContentLength(t, resp2, expected)
	assertBackendHits(1, 1, 1)

	// The fragments are cached like any other resource.
	resp3, _ := tester.AssertGetResponse(`http://localhost:9080/esi-fragments/footer`, 200, "<footer>v1</footer>")
	assertCacheHit(t, resp3)
	assertBackendHits(1, 1, 1)

	// Once the header expires, the page still comes from the cache, along with
	// the current header.
	time.Sleep(2 * time.Second)
	expected = "<header>v2</header><main>content</main><footer>v1</footer>"
	resp4, _ := tester.AssertGetResponse(`http://localhost:9080/esi-fragments/page`, 200, expected)
	assertCacheHit(t, resp4)
	assertContentLength(t, resp4, expected)
	assertBackendHits(1, 2, 1)
}

func TestESILargeFragments(t *testing.T) {
	fragment := strings.Repeat("<p>A paragraph of a fragment way larger than the tag including it.</p>\n", 8*1024)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/esi-large/page":
			_, _ = w.Write([]byte(`<html><esi:include src="fragment"/><hr><esi:include src="fragment"/><hr><esi:include src="fragment"/></html>`))
		case "/esi-large/fragment":
			_, _ = w.Write([]byte(fragment))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	tester := caddytest.NewTester(t)
	tester.InitServer(fmt.Sprintf(`
	{
		admin localhost:2999
		http_port     9080
		https_port    9443
		cache {
			esi
		}
	}
	localhost:9080 {
		route /esi-large/* {
			cache
			reverse_proxy %s
		}
	}`, backend.Listener.Addr()), "caddyfile")

	expected := "<html>" + fragment + "<hr>" + fragment + "<hr>" + fragment + "</html>"
	// Assembled from the backend responses first, then from the cache.
	for i := 0; i < 2; i++ {
		resp, body := tester.AssertGetResponse(`http://localhost:9080/esi-large/page`, 200, expected)
		if len(body) != len(expected) {
			t.Fatalf("expected a body of %d bytes, got %d", len(expected), len(body))
		}
		assertContentLength(t, resp, expected)
	}
}

func TestESIOptInAndLoops(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		admin localhost:2999
		http_port     9080
		https_port    9443
		cache
	}
	localhost:9080 {
		route /esi-disabled {
			cache
			respond "<esi:include src=\"/esi-fragment\"/>"
		}
		route /esi-enabled {
			cache {
				esi
			}
			respond "<esi:include src=\"/esi-fragment\"/>"
		}
		route /esi-loop {
			cache {
				esi
			}
			respond "[<esi:include src=\"/esi-loop\"/>]"
		}
		route /esi-fragment {
			respond "fragment"
		}
	}`, "caddyfile")

	_, _ = tester.AssertGetResponse(`http://localhost:9080/esi-disabled`, 200, `<esi:include src="/esi-fragment"/>`)
	_, _ = tester.AssertGetResponse(`http://localhost:9080/esi-enabled`, 200, "fragment")

	// A page including itself ends up being refused the include, once nested
	// as deep as includes may be.
	done := make(chan struct{})
	go func() {
		defer close(done)

		for i := 0; i < 2; i++ {
			_, _ = tester.AssertGetResponse(`http://localhost:9080/esi-loop`, 200, strings.Repeat("[", 6)+strings.Repeat("]", 6))
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a page including itself should not be included forever")
	}
}

// The cache has to get the page before it is compressed to resolve its ESI
// tags, compression coming once the page is assembled. Hence encode running
// before the cache, which is not the default order.
func TestESIWithCompression(t *testing.T) {
	fragment := strings.Repeat("A compressible fragment. ", 100)
	content := strings.Repeat("Content long enough to be compressed on its own. ", 20)

	tester := caddytest.NewTester(t)
	tester.InitServer(fmt.Sprintf(`
	{
		admin localhost:2999
		http_port     9080
		https_port    9443
		order encode before cache
		cache {
			esi
		}
	}
	localhost:9080 {
		encode gzip
		cache
		respond /esi-compressed/fragment "%s"
		respond /esi-compressed/page "<main><esi:include src=\"fragment\"/>%s</main>"
	}`, fragment, content), "caddyfile")

	expected := "<main>" + fragment + content + "</main>"
	for i := 0; i < 2; i++ {
		resp, _ := tester.AssertGetResponse(`http://localhost:9080/esi-compressed/page`, 200, expected)
		if !resp.Uncompressed {
			t.Errorf("the assembled page should be served compressed")
		}
	}
}
