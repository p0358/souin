package esi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fragments serves the fragments of the tests: each path answers its own
// name, unless a handler is registered for it.
func fragments(handlers map[string]http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler, ok := handlers[r.URL.Path]; ok {
			handler(w, r)

			return
		}

		if strings.HasPrefix(r.URL.Path, "/missing") {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write([]byte(strings.TrimPrefix(r.URL.Path, "/")))
	})
}

func localProcessor(handler http.Handler) *Processor {
	return &Processor{Local: func(*http.Request) http.Handler { return handler }}
}

func process(t *testing.T, p *Processor, rq *http.Request, body string) (string, http.Header) {
	t.Helper()

	header := http.Header{}
	header.Set("Content-Length", fmt.Sprint(len(body)))
	result := p.Process(rq, header, []byte(body))

	return string(result), header
}

func TestProcessLeavesBodiesWithoutMarkupAsIs(t *testing.T) {
	p := localProcessor(fragments(nil))
	rq := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)

	header := http.Header{}
	header.Set("Content-Length", "21")
	header.Set("Etag", `"v1"`)
	body := []byte("<p>Hello <b>you</b></p>")

	if result := p.Process(rq, header, body); &result[0] != &body[0] {
		t.Errorf("a body without markup should be returned as is, got %q", result)
	}
	if header.Get("Content-Length") != "21" || header.Get("Etag") != `"v1"` {
		t.Errorf("the headers of a body without markup should be left alone, got %v", header)
	}

	gzipped := http.Header{"Content-Encoding": {"gzip"}}
	encoded := []byte(`<esi:include src="/name"/>`)
	if result := p.Process(rq, gzipped, encoded); string(result) != string(encoded) {
		t.Errorf("an encoded body should be returned as is, got %q", result)
	}
}

func TestInclude(t *testing.T) {
	p := localProcessor(fragments(map[string]http.HandlerFunc{
		"/query": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(r.URL.RawQuery))
		},
	}))

	for name, tc := range map[string]struct{ template, expected string }{
		"relative to the root": {`Hello <esi:include src="/name"/>!`, "Hello name!"},
		"relative to the page": {`Hello <esi:include src="name"/>!`, "Hello dir/name!"},
		"absolute":             {`Hello <esi:include src="http://example.com/name"/>!`, "Hello name!"},
		"single quotes":        {`Hello <esi:include src='/name' />!`, "Hello name!"},
		"unquoted":             {`Hello <esi:include src=/name/>!`, "Hello name!"},
		"with an end tag":      {`Hello <esi:include src="/name"></esi:include>!`, "Hello name!"},
		"without an end tag":   {`Hello <esi:include src="/name">!`, "Hello name!"},
		"escaped attribute":    {`<esi:include src="/query?a=1&amp;b=2"/>`, "a=1&b=2"},
		"spread over lines":    {"<esi:include\n\tsrc=\"/name\"\n/>", "name"},
		"several":              {`<esi:include src="/a"/>, <esi:include src="/b"/>`, "a, b"},
	} {
		t.Run(name, func(t *testing.T) {
			rq := httptest.NewRequest(http.MethodGet, "http://example.com/dir/page", nil)
			if result, _ := process(t, p, rq, tc.template); result != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, result)
			}
		})
	}
}

func TestIncludeDescribesTheAssembledBody(t *testing.T) {
	p := localProcessor(fragments(nil))
	rq := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)

	header := http.Header{}
	header.Set("Content-Length", "26")
	header.Set("Content-Type", "text/html")
	header.Set("Etag", `"template"`)
	header.Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	result := p.Process(rq, header, []byte(`<esi:include src="/fragment"/>`))

	if string(result) != "fragment" {
		t.Fatalf("unexpected body %q", result)
	}
	if header.Get("Content-Length") != "8" {
		t.Errorf("the Content-Length should be the one of the assembled body, got %q", header.Get("Content-Length"))
	}
	if header.Get("Etag") != "" || header.Get("Last-Modified") != "" {
		t.Errorf("the validators of the template should not stand for the assembled body, got %v", header)
	}
	if header.Get("Content-Type") != "text/html" {
		t.Errorf("the other headers should be kept, got %v", header)
	}
}

func TestIncludeGrowsTheBodyAsMuchAsNeeded(t *testing.T) {
	large := strings.Repeat("0123456789abcdef", 64*1024)
	p := localProcessor(fragments(map[string]http.HandlerFunc{
		"/large": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(large))
		},
	}))
	rq := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)

	template := []byte(`<header><esi:include src="/large"/></header><main>x</main><footer><esi:include src="/large"/></footer>`)
	original := string(template)
	header := http.Header{}
	header.Set("Content-Length", fmt.Sprint(len(template)))
	result := p.Process(rq, header, template)

	expected := "<header>" + large + "</header><main>x</main><footer>" + large + "</footer>"
	if string(result) != expected {
		t.Fatalf("unexpected body of %d bytes, expected %d", len(result), len(expected))
	}
	if header.Get("Content-Length") != fmt.Sprint(len(expected)) {
		t.Errorf("expected a Content-Length of %d, got %s", len(expected), header.Get("Content-Length"))
	}
	if string(template) != original {
		t.Errorf("the template should be left untouched, got %q", template)
	}
}

func TestIncludeFailures(t *testing.T) {
	p := localProcessor(fragments(map[string]http.HandlerFunc{
		"/error": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("error page"))
		},
		"/redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/name", http.StatusFound)
		},
		"/gzipped": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte("gzipped"))
			_ = gz.Close()
		},
		"/empty": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	}))

	for name, tc := range map[string]struct{ template, expected string }{
		"not found":               {`a<esi:include src="/missing"/>b`, "ab"},
		"server error":            {`a<esi:include src="/error"/>b`, "ab"},
		"redirect":                {`a<esi:include src="/redirect"/>b`, "ab"},
		"encoded":                 {`a<esi:include src="/gzipped"/>b`, "ab"},
		"no src":                  {`a<esi:include alt="/missing"/>b`, "ab"},
		"unsupported scheme":      {`a<esi:include src="file:///etc/passwd"/>b`, "ab"},
		"alt":                     {`a<esi:include src="/missing" alt="/name"/>b`, "anameb"},
		"alt failing too":         {`a<esi:include src="/missing" alt="/missing-too"/>b`, "ab"},
		"onerror continue":        {`a<esi:include src="/missing" onerror="continue"/>b`, "ab"},
		"empty fragment":          {`a<esi:include src="/empty"/>b`, "ab"},
		"the rest is still there": {`a<esi:include src="/missing"/>b<esi:include src="/name"/>c`, "abnamec"},
	} {
		t.Run(name, func(t *testing.T) {
			rq := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)
			if result, _ := process(t, p, rq, tc.template); result != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, result)
			}
		})
	}
}

func TestTry(t *testing.T) {
	p := localProcessor(fragments(nil))

	for name, tc := range map[string]struct{ template, expected string }{
		"attempt": {
			`<esi:try><esi:attempt>[<esi:include src="/name"/>]</esi:attempt><esi:except>fallback</esi:except></esi:try>`,
			"[name]",
		},
		"except": {
			`<esi:try><esi:attempt>[<esi:include src="/name"/><esi:include src="/missing"/>]</esi:attempt><esi:except>fallback <esi:include src="/name"/></esi:except></esi:try>`,
			"fallback name",
		},
		"onerror continue in the attempt": {
			`<esi:try><esi:attempt>[<esi:include src="/missing" onerror="continue"/>]</esi:attempt><esi:except>fallback</esi:except></esi:try>`,
			"[]",
		},
		"nested try handling the failure": {
			`<esi:try><esi:attempt>[<esi:try><esi:attempt><esi:include src="/missing"/></esi:attempt><esi:except>inner</esi:except></esi:try>]</esi:attempt><esi:except>outer</esi:except></esi:try>`,
			"[inner]",
		},
		"nested try failing": {
			`<esi:try><esi:attempt>[<esi:try><esi:attempt><esi:include src="/missing"/></esi:attempt><esi:except><esi:include src="/missing"/></esi:except></esi:try>]</esi:attempt><esi:except>outer</esi:except></esi:try>`,
			"outer",
		},
		"surrounded": {
			"a\n<esi:try>\n  <esi:attempt><esi:include src=\"/missing\"/></esi:attempt>\n  <esi:except>b</esi:except>\n</esi:try>\nc",
			"a\nb\nc",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rq := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)
			if result, _ := process(t, p, rq, tc.template); result != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, result)
			}
		})
	}
}

func TestOtherMarkup(t *testing.T) {
	var requested atomic.Int32
	p := localProcessor(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Add(1)
		_, _ = w.Write([]byte(strings.TrimPrefix(r.URL.Path, "/")))
	}))

	for name, tc := range map[string]struct {
		template, expected string
		requests           int32
	}{
		"remove": {
			`a<esi:remove><a href="/name">name</a></esi:remove>b`, "ab", 0,
		},
		"comment": {
			`a<esi:comment text="for the developers"/>b`, "ab", 0,
		},
		"esi comment": {
			"a<!--esi\n<esi:include src=\"/name\"/>\n-->b", "a\nname\nb", 1,
		},
		"html comment": {
			`a<!-- <esi:include src="/name"/> -->b`, `a<!-- <esi:include src="/name"/> -->b`, 0,
		},
		"cdata": {
			`a<![CDATA[<esi:include src="/name"/>]]>b`, `a<![CDATA[<esi:include src="/name"/>]]>b`, 0,
		},
		"unsupported element": {
			`a<esi:choose><esi:when test="$(HTTP_HOST)=='example.com'"><esi:include src="/name"/></esi:when></esi:choose>b`,
			`a<esi:choose><esi:when test="$(HTTP_HOST)=='example.com'"><esi:include src="/name"/></esi:when></esi:choose>b`,
			0,
		},
		"unclosed tag": {
			`a<esi:include src="/name"`, `a<esi:include src="/name"`, 0,
		},
		"unclosed attribute": {
			`a<esi:include src="/name/>b`, `a<esi:include src="/name/>b`, 0,
		},
		"unclosed remove": {
			`a<esi:remove>b<esi:include src="/name"/>`, `a<esi:remove>bname`, 1,
		},
		"unclosed esi comment": {
			`a<!--esi <esi:include src="/name"/>`, `a<!--esi <esi:include src="/name"/>`, 0,
		},
		"not a tag": {
			`a<esi:>b<esi: include>c<esi:include"/>d`, `a<esi:>b<esi: include>c<esi:include"/>d`, 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			requested.Store(0)
			rq := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)
			if result, _ := process(t, p, rq, tc.template); result != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, result)
			}
			if requested.Load() != tc.requests {
				t.Errorf("expected %d fragment requests, got %d", tc.requests, requested.Load())
			}
		})
	}
}

func TestNestedIncludes(t *testing.T) {
	var requested atomic.Int32
	handlers := map[string]http.HandlerFunc{
		"/header": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<header><esi:include src="menu"/></header>`))
		},
		"/loop": func(w http.ResponseWriter, _ *http.Request) {
			requested.Add(1)
			_, _ = w.Write([]byte(`[<esi:include src="/loop"/>]`))
		},
	}
	p := localProcessor(fragments(handlers))

	rq := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)
	if result, _ := process(t, p, rq, `<esi:include src="/header"/>`); result != "<header>menu</header>" {
		t.Errorf("the includes of a fragment should be resolved, got %q", result)
	}

	done := make(chan string)
	go func() {
		result, _ := process(t, p, rq, `<esi:include src="/loop"/>`)
		done <- result
	}()

	select {
	case result := <-done:
		expected := strings.Repeat("[", maxDepth) + strings.Repeat("]", maxDepth)
		if result != expected {
			t.Errorf("expected %q, got %q", expected, result)
		}
		if requested.Load() != maxDepth {
			t.Errorf("expected %d requests of the looping fragment, got %d", maxDepth, requested.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a fragment including itself should not be included forever")
	}

	// The depth the request already is at counts.
	requested.Store(0)
	nested := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)
	nested.Header.Set(DepthHeader, fmt.Sprint(maxDepth-1))
	if result, _ := process(t, p, nested, `<esi:include src="/loop"/>`); result != "[]" || requested.Load() != 1 {
		t.Errorf("expected a single level of include, got %q after %d requests", result, requested.Load())
	}

	// And a forged negative depth doesn't lift the limit.
	requested.Store(0)
	forged := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)
	forged.Header.Set(DepthHeader, "-1000")
	_, _ = process(t, p, forged, `<esi:include src="/loop"/>`)
	if requested.Load() != maxDepth {
		t.Errorf("expected %d requests of the looping fragment, got %d", maxDepth, requested.Load())
	}
}

func TestLocalSubrequest(t *testing.T) {
	var subrequest *http.Request
	p := localProcessor(fragments(map[string]http.HandlerFunc{
		"/inspect": func(w http.ResponseWriter, r *http.Request) {
			subrequest = r
			_, _ = w.Write([]byte("ok"))
		},
	}))

	rq := httptest.NewRequest(http.MethodGet, "https://example.com/page?a=b", nil)
	rq.TLS = &tls.ConnectionState{}
	rq.RemoteAddr = "192.0.2.1:1234"
	rq.Header.Set("Cookie", "session=1")
	rq.Header.Set("Accept-Language", "fr")
	rq.Header.Set("Accept-Encoding", "gzip")
	rq.Header.Set("If-None-Match", `"v1"`)
	rq.Header.Set("If-Modified-Since", "Mon, 02 Jan 2006 15:04:05 GMT")

	if result, _ := process(t, p, rq, `<esi:include src="/inspect?c=d"/>`); result != "ok" {
		t.Fatalf("unexpected body %q", result)
	}

	if subrequest.Method != http.MethodGet || subrequest.Host != "example.com" || subrequest.URL.Host != "" ||
		subrequest.URL.Path != "/inspect" || subrequest.URL.RawQuery != "c=d" || subrequest.RequestURI != "/inspect?c=d" {
		t.Errorf("unexpected subrequest target %s %s %s (%s)", subrequest.Method, subrequest.Host, subrequest.URL, subrequest.RequestURI)
	}
	if subrequest.TLS == nil || subrequest.RemoteAddr != "192.0.2.1:1234" {
		t.Errorf("the subrequest should look like it comes from the same connection, got %v %s", subrequest.TLS, subrequest.RemoteAddr)
	}
	if subrequest.Header.Get("Cookie") != "session=1" || subrequest.Header.Get("Accept-Language") != "fr" {
		t.Errorf("the subrequest should be made on behalf of the client, got %v", subrequest.Header)
	}
	if subrequest.Header.Get("If-None-Match") != "" || subrequest.Header.Get("If-Modified-Since") != "" {
		t.Errorf("the subrequest should ask for the whole fragment, got %v", subrequest.Header)
	}
	if subrequest.Header.Get("Accept-Encoding") != "identity" || subrequest.Header.Get(DepthHeader) != "1" {
		t.Errorf("unexpected subrequest headers %v", subrequest.Header)
	}
	if rq.Header.Get("If-None-Match") == "" || rq.Header.Get(DepthHeader) != "" {
		t.Errorf("the request being served should be left untouched, got %v", rq.Header)
	}
}

func TestRemoteIncludes(t *testing.T) {
	var subrequest *http.Request
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subrequest = r
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte("remote " + r.URL.Path))
		_ = gz.Close()
	}))
	defer other.Close()

	// Without a local handler, even the same origin is requested over the
	// network, the way the client would.
	for name, tc := range map[string]struct {
		processor  *Processor
		host       string
		credential string
	}{
		"other origin":                 {localProcessor(fragments(nil)), "example.com", ""},
		"same origin, no local server": {&Processor{}, strings.TrimPrefix(other.URL, "http://"), "session=1"},
	} {
		t.Run(name, func(t *testing.T) {
			rq := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/page", nil)
			rq.Header.Set("Cookie", "session=1")
			rq.Header.Set("Accept-Language", "fr")

			if result, _ := process(t, tc.processor, rq, `<esi:include src="`+other.URL+`/fragment"/>`); result != "remote /fragment" {
				t.Fatalf("unexpected body %q", result)
			}
			if subrequest.Header.Get("Cookie") != tc.credential {
				t.Errorf("expected the Cookie %q, got %q", tc.credential, subrequest.Header.Get("Cookie"))
			}
			if subrequest.Header.Get("Accept-Language") != "fr" || subrequest.Header.Get(DepthHeader) != "1" {
				t.Errorf("unexpected subrequest headers %v", subrequest.Header)
			}
		})
	}
}

func TestIncludesAreBoundToTheRequest(t *testing.T) {
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stalled.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	rq := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil).WithContext(ctx)

	start := time.Now()
	result, _ := process(t, &Processor{}, rq, `a<esi:include src="`+stalled.URL+`/stalled"/>b`)
	if result != "ab" {
		t.Errorf("expected the include to be dropped, got %q", result)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the include should give up with the request, it took %v", elapsed)
	}
}

func TestParseStartTag(t *testing.T) {
	for _, tc := range []struct {
		input, rest string
		expected    startTag
	}{
		{
			`<esi:include src="/a" alt='/b' onerror=continue/>`, "",
			startTag{name: "include", attrs: map[string]string{"src": "/a", "alt": "/b", "onerror": "continue"}, empty: true},
		},
		{`<esi:try>rest`, "rest", startTag{name: "try", attrs: map[string]string{}}},
		{`<esi:include SRC="/a>b" />rest`, "rest", startTag{name: "include", attrs: map[string]string{"src": "/a>b"}, empty: true}},
		{`<esi:include src="/a" src="/b"/>`, "", startTag{name: "include", attrs: map[string]string{"src": "/a"}, empty: true}},
	} {
		tag, ok := parseStartTag([]byte(tc.input))
		if !ok || tag.name != tc.expected.name || tag.empty != tc.expected.empty ||
			tag.length != len(tc.input)-len(tc.rest) || fmt.Sprint(tag.attrs) != fmt.Sprint(tc.expected.attrs) {
			t.Errorf("unexpected parsing of %s: %+v (%v)", tc.input, tag, ok)
		}
	}

	for _, input := range []string{`<esi:`, `<esi:include`, `<esi:include src="/a`, `<esi:include =/a/>`, `<esi:include / >`, `<esi:1nclude"/>`} {
		if tag, ok := parseStartTag([]byte(input)); ok {
			t.Errorf("%s should not parse, got %+v", input, tag)
		}
	}
}

func TestElementContent(t *testing.T) {
	b := []byte(`<esi:try>a<esi:try>b</esi:try >c<esi:try/>d</esi:try>e`)
	tag, _ := parseStartTag(b)
	content, end, ok := elementContent(b, tag)
	if !ok || string(content) != `a<esi:try>b</esi:try >c<esi:try/>d` || string(b[end:]) != "e" {
		t.Errorf("unexpected content %q, rest %q (%v)", content, b[end:], ok)
	}

	if !bytes.Equal(b, []byte(`<esi:try>a<esi:try>b</esi:try >c<esi:try/>d</esi:try>e`)) {
		t.Errorf("the input should be left untouched")
	}
}

func TestIncludesDontConnectToTheRequestHost(t *testing.T) {
	var requested atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested.Add(1)
		_, _ = w.Write([]byte("secret"))
	}))
	defer internal.Close()

	// A client may claim to reach any host, without a local handler to serve
	// it, the includes resolved against it are not fetched.
	rq := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)
	rq.Host = strings.TrimPrefix(internal.URL, "http://")

	for _, template := range []string{`a<esi:include src="/secret"/>b`, `a<esi:include src="secret"/>b`} {
		if result, _ := process(t, &Processor{}, rq, template); result != "ab" {
			t.Errorf("expected the include to be dropped, got %q", result)
		}
	}
	if requested.Load() != 0 {
		t.Errorf("the host of the request should not be connected to, it got %d requests", requested.Load())
	}

	// The hosts named by the template are.
	if result, _ := process(t, &Processor{}, rq, `a<esi:include src="`+internal.URL+`/secret"/>b`); result != "asecretb" {
		t.Errorf("expected the include to be resolved, got %q", result)
	}
}

func TestCommentedMarkupLeavesTheHeadersAsIs(t *testing.T) {
	p := localProcessor(fragments(nil))
	rq := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)

	header := http.Header{}
	header.Set("Content-Length", "35")
	header.Set("Etag", `"v1"`)
	body := []byte(`<!-- <esi:include src="/name"/> -->`)

	if result := p.Process(rq, header, body); string(result) != string(body) {
		t.Errorf("unexpected body %q", result)
	}
	if header.Get("Content-Length") != "35" || header.Get("Etag") != `"v1"` {
		t.Errorf("the headers of an unchanged body should be left alone, got %v", header)
	}
}
