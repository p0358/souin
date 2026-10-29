// Package esi resolves the Edge Side Includes markup
// (https://www.w3.org/TR/esi-lang/) of the responses served by the cache.
// The stored response keeps its markup and gets it resolved every time it is
// served, so a cached page always shows the current version of the fragments
// it includes, which are cached on their own.
package esi

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/darkweak/storages/core"
)

// DepthHeader tells the handler serving an include how deep in includes it
// is nested. Includes nested deeper than maxDepth are refused, which is what
// ends a fragment including itself, directly or not.
const DepthHeader = "X-Souin-Esi-Depth"

// maxDepth is as many levels as Varnish and Akamai resolve by default.
const maxDepth = 5

var (
	tagOpen        = []byte("<esi:")
	esiCommentOpen = []byte("<!--esi")
	commentOpen    = []byte("<!--")
	commentClose   = []byte("-->")
	cdataOpen      = []byte("<![CDATA[")
	cdataClose     = []byte("]]>")
)

// storedRepresentationHeaders identify the stored bytes, which the assembled
// body is not: a validator answering a conditional request for it would keep
// a client on the fragments it got the first time.
var storedRepresentationHeaders = []string{"Etag", "Last-Modified", "Content-Md5", "Digest", "Content-Digest", "Repr-Digest"}

var (
	errMissingSrc = errors.New("no src attribute")
	errTooDeep    = errors.New("includes are nested too deep")
)

// HasMarkup reports whether b carries any ESI markup.
func HasMarkup(b []byte) bool {
	return bytes.Contains(b, tagOpen) || bytes.Contains(b, esiCommentOpen)
}

// Processor resolves the ESI markup of response bodies. It supports
// esi:include (with its alt and onerror attributes), esi:remove,
// esi:comment, esi:try and the <!--esi --> comments. The other elements are
// left as they are.
type Processor struct {
	// Local returns the handler serving the origin of the given request, if
	// any. The includes targeting that origin are then served in process,
	// through the same handlers, and therefore the same cache, as a client
	// request would be, instead of over the network.
	Local func(*http.Request) http.Handler
	// Client fetches the includes that Local doesn't serve. It defaults to
	// http.DefaultClient.
	Client *http.Client
	Logger core.Logger

	encodedWarning sync.Once
}

// Process returns body with its ESI markup resolved for rq, and updates
// header, which describes body, to describe the result instead. A body
// without markup, or with its markup hidden by a content encoding, is
// returned as is.
func (p *Processor) Process(rq *http.Request, header http.Header, body []byte) []byte {
	if encoding := header.Get("Content-Encoding"); !isIdentity(encoding) {
		// Most likely, the response was compressed before reaching the cache,
		// which then serves the ESI tags as they are. Say so once.
		p.encodedWarning.Do(func() {
			p.warnf("The ESI tags of %s encoded responses such as %s cannot be resolved: the cache has to get them before they are compressed", encoding, rq.URL)
		})
		p.debugf("Skip the ESI markup of %s, its body is %s encoded", rq.URL, encoding)

		return body
	}

	if !HasMarkup(body) {
		return body
	}

	base := requestURL(rq)
	j := &job{Processor: p, rq: rq, origin: url.URL{Scheme: base.Scheme, Host: base.Host}}

	// The buffer starts with the size of the template and grows with what
	// gets included.
	out := bytes.NewBuffer(make([]byte, 0, len(body)))
	j.process(out, body, base, requestDepth(rq.Header))
	result := out.Bytes()

	// Markup in comments only leaves the body as it was, and so its headers.
	if bytes.Equal(result, body) {
		return body
	}

	header.Set("Content-Length", strconv.Itoa(len(result)))
	for _, name := range storedRepresentationHeaders {
		header.Del(name)
	}

	return result
}

func (p *Processor) debugf(format string, args ...interface{}) {
	if p.Logger != nil {
		p.Logger.Debugf(format, args...)
	}
}

func (p *Processor) warnf(format string, args ...interface{}) {
	if p.Logger != nil {
		p.Logger.Warnf(format, args...)
	}
}

// job resolves the markup of the body served to one request.
type job struct {
	*Processor
	rq     *http.Request
	origin url.URL
}

// process writes b to out with its ESI markup resolved. Includes are resolved
// relative to base, depth being how deep b is nested in includes. It reports
// whether an include failed, which makes an enclosing esi:try fall back on its
// esi:except.
func (j *job) process(out *bytes.Buffer, b []byte, base *url.URL, depth int) bool {
	failed := false

	for {
		i := nextMarkup(b)
		if i < 0 {
			break
		}

		out.Write(b[:i])
		b = b[i:]

		var n int
		switch {
		case bytes.HasPrefix(b, esiCommentOpen):
			// A <!--esi --> comment hides markup from the clients that don't
			// process it: the comment goes away and what it holds is processed.
			content := b[len(esiCommentOpen):]
			end := bytes.Index(content, commentClose)
			if end < 0 {
				out.Write(b)

				return failed
			}

			failed = j.process(out, content[:end], base, depth) || failed
			n = len(esiCommentOpen) + end + len(commentClose)
		case bytes.HasPrefix(b, commentOpen):
			// Markup in a regular comment or a CDATA section is commented out
			// or escaped: it is not processed.
			n = through(b, len(commentOpen), commentClose)
			out.Write(b[:n])
		case bytes.HasPrefix(b, cdataOpen):
			n = through(b, len(cdataOpen), cdataClose)
			out.Write(b[:n])
		default:
			var elementFailed bool
			n, elementFailed = j.element(out, b, base, depth)
			failed = elementFailed || failed
		}

		b = b[n:]
	}

	out.Write(b)

	return failed
}

// element resolves the ESI element b starts with into out. It returns the
// length of the element in b, and whether an include failed.
func (j *job) element(out *bytes.Buffer, b []byte, base *url.URL, depth int) (int, bool) {
	tag, ok := parseStartTag(b)
	if !ok {
		out.Write(tagOpen)

		return len(tagOpen), false
	}

	switch tag.name {
	case "include":
		return emptyElementLength(b, tag), j.include(out, tag.attrs, base, depth)
	case "comment":
		return emptyElementLength(b, tag), false
	case "remove":
		if _, end, closed := elementContent(b, tag); closed {
			return end, false
		}
	case "try":
		if content, end, closed := elementContent(b, tag); closed {
			return end, j.try(out, content, base, depth)
		}
	default:
		// The other elements (esi:choose, esi:vars, esi:inline...) are not
		// supported. They are left as they are, along with what they hold,
		// rather than half processed.
		j.debugf("Leave the unsupported esi:%s element of %s as is", tag.name, j.rq.URL)

		if _, end, closed := elementContent(b, tag); closed {
			out.Write(b[:end])

			return end, false
		}
	}

	// An element missing its end tag is no markup.
	out.Write(b[:tag.length])

	return tag.length, false
}

// emptyElementLength returns the length in b of an element meant to be empty,
// along with its end tag, if it was given one anyway.
func emptyElementLength(b []byte, tag startTag) int {
	if tag.empty {
		return tag.length
	}

	i := skipSpaces(b, tag.length)
	if n := endTagLength(b[i:], tag.name); n > 0 {
		return i + n
	}

	return tag.length
}

// try resolves an esi:try into out: its esi:attempt, or its esi:except when an
// include of the attempt failed.
func (j *job) try(out *bytes.Buffer, content []byte, base *url.URL, depth int) bool {
	var attempt, except []byte

	for {
		i := bytes.Index(content, tagOpen)
		if i < 0 {
			break
		}
		content = content[i:]

		tag, ok := parseStartTag(content)
		if !ok {
			content = content[len(tagOpen):]

			continue
		}

		inner, end, closed := elementContent(content, tag)
		if !closed {
			content = content[tag.length:]

			continue
		}

		switch tag.name {
		case "attempt":
			attempt = inner
		case "except":
			except = inner
		}
		content = content[end:]
	}

	mark := out.Len()
	if !j.process(out, attempt, base, depth) {
		return false
	}

	out.Truncate(mark)

	return j.process(out, except, base, depth)
}

// include writes to out the fragment an esi:include points at, or its alt
// when the fragment cannot be fetched. It reports whether neither could be,
// unless the include tolerates it with onerror="continue".
func (j *job) include(out *bytes.Buffer, attrs map[string]string, base *url.URL, depth int) bool {
	err := errMissingSrc

	for _, ref := range []string{attrs["src"], attrs["alt"]} {
		if ref == "" {
			continue
		}

		var target *url.URL
		var body []byte
		if target, body, err = j.fetch(ref, base, depth+1); err == nil {
			// The fragment may carry markup of its own, relative to it. Once
			// the handler that served it resolved it, there is none left.
			j.process(out, body, target, depth+1)

			return false
		}

		j.debugf("Cannot fetch the ESI include %s of %s: %v", ref, j.rq.URL, err)
	}

	if strings.EqualFold(attrs["onerror"], "continue") {
		return false
	}

	j.warnf("Cannot resolve the ESI include %s of %s: %v", attrs["src"], j.rq.URL, err)

	return true
}

// nextMarkup returns the index in b of the first ESI tag, comment or CDATA
// section, or -1 when there is none.
func nextMarkup(b []byte) int {
	for i := 0; ; i++ {
		next := bytes.IndexByte(b[i:], '<')
		if next < 0 {
			return -1
		}

		i += next
		if rest := b[i:]; bytes.HasPrefix(rest, tagOpen) || bytes.HasPrefix(rest, commentOpen) || bytes.HasPrefix(rest, cdataOpen) {
			return i
		}
	}
}

// through returns the length of b up to the end of the first sep after
// b[from:], or of the whole b when there is none.
func through(b []byte, from int, sep []byte) int {
	end := bytes.Index(b[from:], sep)
	if end < 0 {
		return len(b)
	}

	return from + end + len(sep)
}

// requestURL returns the absolute URL rq targets, which the includes of the
// document it gets are relative to.
func requestURL(rq *http.Request) *url.URL {
	u := *rq.URL
	u.Scheme = "http"
	if rq.TLS != nil {
		u.Scheme = "https"
	}

	if rq.Host != "" {
		u.Host = rq.Host
	}

	return &u
}

// requestDepth returns how deep in includes the document requested with
// header is nested.
func requestDepth(header http.Header) int {
	depth, err := strconv.Atoi(header.Get(DepthHeader))
	if err != nil || depth < 0 {
		return 0
	}

	return depth
}

func isIdentity(encoding string) bool {
	return encoding == "" || strings.EqualFold(encoding, "identity")
}
