package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/contentsquare/chproxy/chdecompressor"
	"github.com/contentsquare/chproxy/log"
)

func respondWith(rw http.ResponseWriter, err error, status int) {
	log.ErrorWithCallDepth(err, 1)
	rw.WriteHeader(status)
	fmt.Fprintf(rw, "%s\n", err)
}

var defaultUser = "default"

// getAuth retrieves auth credentials from request
// according to CH documentation @see "https://clickhouse.yandex/docs/en/interfaces/http/"
func getAuth(req *http.Request) (string, string) {
	// check X-ClickHouse- headers
	name := req.Header.Get("X-ClickHouse-User")
	pass := req.Header.Get("X-ClickHouse-Key")
	if name != "" {
		return name, pass
	}
	// if header is empty - check basicAuth
	if name, pass, ok := req.BasicAuth(); ok {
		return name, pass
	}
	// if basicAuth is empty - check URL params `user` and `password`
	params := req.URL.Query()
	if name := params.Get("user"); name != "" {
		pass := params.Get("password")
		return name, pass
	}
	// if still no credentials - treat it as `default` user request
	return defaultUser, ""
}

// getSessionId retrieves session id
func getSessionId(req *http.Request) string {
	params := req.URL.Query()
	sessionId := params.Get("session_id")
	return sessionId
}

// getSessionId retrieves session id
func getSessionTimeout(req *http.Request) int {
	params := req.URL.Query()
	sessionTimeout, err := strconv.Atoi(params.Get("session_timeout"))
	if err == nil && sessionTimeout > 0 {
		return sessionTimeout
	}
	return 60
}

// getQuerySnippet returns query snippet.
//
// getQuerySnippet must be called only for error reporting.
func getQuerySnippet(req *http.Request) string {
	query := req.URL.Query().Get("query")
	body := getQuerySnippetFromBody(req)

	if len(query) != 0 && len(body) != 0 {
		query += "\n"
	}

	return query + body
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

func getQuerySnippetFromBody(req *http.Request) string {
	if req.Body == nil {
		return ""
	}

	crc, ok := req.Body.(*cachedReadCloser)
	if !ok {
		crc = &cachedReadCloser{
			ReadCloser: req.Body,
		}
	}

	// 'read' request body, so it traps into to crc.
	// Ignore any errors, since getQuerySnippet is called only
	// during error reporting.
	// Temporary solution: Quick and dirty way to work with the request body.
	// TODO: Create an original copy of req.Body and work with the copy to avoid altering the original request.
	// This current approach consumes the req.Body content with io.Copy(io.Discard, crc) to reset the internal state of crc.
	// However, it is not the most efficient or safest method, as it modifies the original req.Body.
	io.Copy(io.Discard, crc) // nolint
	data := crc.String()

	// Here, we attempt to restore req.Body by wrapping the string data in a ReadCloser.
	// This is part of the temporary solution and should be replaced with a more robust method that does not consume the original req.Body.
	req.Body = io.NopCloser(strings.NewReader(data))

	u := getDecompressor(req)
	if u == nil {
		return data
	}
	bs := bytes.NewBufferString(data)
	b, err := u.decompress(bs)
	if err == nil {
		return string(b)
	}
	// It is better to return partially decompressed data instead of an empty string.
	if len(b) > 0 {
		return string(b)
	}

	// The data failed to be decompressed. Return compressed data
	// instead of an empty string.
	return data
}

// getFullQuery returns full query from req.
func getFullQuery(req *http.Request) ([]byte, error) {
	var result bytes.Buffer

	if req.URL.Query().Get("query") != "" {
		result.WriteString(req.URL.Query().Get("query"))
	}

	body, err := getFullQueryFromBody(req)
	if err != nil {
		return nil, err
	}

	if result.Len() != 0 && len(body) != 0 {
		result.WriteByte('\n')
	}

	result.Write(body)

	return result.Bytes(), nil
}

func getFullQueryFromBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}

	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	// restore body for further reading
	req.Body = io.NopCloser(bytes.NewBuffer(data))
	u := getDecompressor(req)
	if u == nil {
		return data, nil
	}
	br := bytes.NewReader(data)
	b, err := u.decompress(br)
	if err != nil {
		return nil, fmt.Errorf("cannot uncompress query: %w", err)
	}

	return b, nil
}

var cachableStatements = []string{"SELECT", "WITH"}

// canCacheQuery returns true if q can be cached.
func canCacheQuery(q []byte) bool {
	q = skipLeadingComments(q)

	for _, statement := range cachableStatements {
		if len(q) < len(statement) {
			continue
		}

		l := bytes.ToUpper(q[:len(statement)])
		if bytes.HasPrefix(l, []byte(statement)) {
			return true
		}
	}

	return false
}

//nolint:cyclop // No clean way to split this.
func skipLeadingComments(q []byte) []byte {
	for len(q) > 0 {
		switch q[0] {
		case '\t', '\n', '\v', '\f', '\r', ' ':
			q = q[1:]
		case '-':
			if len(q) < 2 || q[1] != '-' {
				return q
			}

			// skip `-- comment`
			n := bytes.IndexByte(q, '\n')
			if n < 0 {
				return nil
			}
			q = q[n+1:]
		case '/':
			if len(q) < 2 || q[1] != '*' {
				return q
			}

			// skip `/* comment */`
			for {
				n := bytes.IndexByte(q, '*')
				if n < 0 {
					return nil
				}
				q = q[n+1:]
				if len(q) == 0 {
					return nil
				}
				if q[0] == '/' {
					q = q[1:]
					break
				}
			}
		default:
			return q
		}
	}
	return nil
}

// splits header string in sorted slice
func sortHeader(header string) string {
	h := strings.Split(header, ",")
	for i, v := range h {
		h[i] = strings.TrimSpace(v)
	}
	sort.Strings(h)
	return strings.Join(h, ",")
}

func getDecompressor(req *http.Request) decompressor {
	if req.Header.Get("Content-Encoding") == "gzip" {
		return gzipDecompressor{}
	}
	if req.URL.Query().Get("decompress") == "1" {
		return chDecompressor{}
	}
	return nil
}

type decompressor interface {
	decompress(r io.Reader) ([]byte, error)
}

type gzipDecompressor struct{}

func (dc gzipDecompressor) decompress(r io.Reader) ([]byte, error) {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("cannot ungzip query: %w", err)
	}
	return io.ReadAll(gr)
}

type chDecompressor struct{}

func (dc chDecompressor) decompress(r io.Reader) ([]byte, error) {
	lr := chdecompressor.NewReader(r)
	return io.ReadAll(lr)
}

func calcMapHash(m map[string]string) (uint32, error) {
	if len(m) == 0 {
		return 0, nil
	}
	var keys []string
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := fnv.New32a()
	for _, k := range keys {
		str := fmt.Sprintf("%s=%s&", k, m[k])
		_, err := h.Write([]byte(str))
		if err != nil {
			return 0, err
		}
	}
	return h.Sum32(), nil
}
func calcCredentialHash(user string, pwd string) (uint32, error) {
	h := fnv.New32a()
	_, err := h.Write([]byte(user + pwd))
	return h.Sum32(), err
}

// Function to read the request body and return it as a byte slice.
// It also restores the req.Body to be used again.
func readAndRestoreRequestBody(req *http.Request) ([]byte, error) {
	// Read the entire request body.
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	// Restore the req.Body with a new reader for the original content.
	req.Body = io.NopCloser(bytes.NewReader(body))

	// Return the read body.
	return body, nil
}
