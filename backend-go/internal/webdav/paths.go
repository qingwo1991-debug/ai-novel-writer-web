package webdav

import (
	"bytes"
	"encoding/xml"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

func controls(s string) bool {
	for _, r := range s {
		if r <= 0x1f || r >= 0x7f && r <= 0x9f {
			return true
		}
	}
	return false
}

// NormalizeEndpoint mirrors the credential-store endpoint contract: HTTPS or
// literal loopback HTTP only, no URL credentials, search or fragment. It is not
// an SSRF allowlist: deployment must authorize every request with DNS policy.
func NormalizeEndpoint(value string) (string, error) {
	s := strings.TrimSpace(value)
	if s == "" || len(s) > 2048 || !utf8.ValidString(s) || controls(s) || strings.ContainsAny(s, "?#\\") {
		return "", InputInvalid
	}
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", InputInvalid
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, "%* \t\r\n") {
		return "", InputInvalid
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return "", InputInvalid
	}
	port := u.Port()
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 0 || p > 65535 {
			return "", InputInvalid
		}
		port = strconv.Itoa(p)
	}
	if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		port = ""
	}
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	// WHATWG collapses literal and percent-encoded dot path segments.
	segments := strings.Split(u.EscapedPath(), "/")
	clean := []string{}
	for _, seg := range segments {
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return "", InputInvalid
		}
		if decoded == "." {
			continue
		}
		if decoded == ".." {
			if len(clean) > 1 {
				clean = clean[:len(clean)-1]
			}
			continue
		}
		clean = append(clean, seg)
	}
	escaped := strings.Join(clean, "/")
	if !strings.HasPrefix(escaped, "/") {
		escaped = "/" + escaped
	}
	if !strings.HasSuffix(escaped, "/") {
		escaped += "/"
	}
	u.Path, err = url.PathUnescape(escaped)
	if err != nil {
		return "", InputInvalid
	}
	u.RawPath = escaped
	return u.String(), nil
}
func component(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.!~*'()", rune(c)) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

type Locations struct {
	Collections []*url.URL
	Generations *url.URL
	Archive     *url.URL
	Manifest    *url.URL
	Completion  *url.URL
}

func GenerationLocations(endpoint, book, generation string) (Locations, error) {
	normalized, err := NormalizeEndpoint(endpoint)
	if err != nil {
		return Locations{}, err
	}
	if !idPattern.MatchString(book) || generation != "" && !uuidPattern.MatchString(generation) {
		return Locations{}, InputInvalid
	}
	u, _ := url.Parse(normalized)
	relative := func(s string) *url.URL { r, _ := url.Parse(s); return u.ResolveReference(r) }
	bookPath := "books/" + component(book) + "/"
	locations := Locations{Collections: []*url.URL{relative("books/"), relative(bookPath), relative(bookPath + "generations/")}, Generations: relative(bookPath + "generations/")}
	if generation == "" {
		generation = "00000000-0000-4000-8000-000000000000"
	} else {
		locations.Collections = append(locations.Collections, relative(bookPath+"generations/"+component(generation)+"/"))
	}
	prefix := bookPath + "generations/" + component(generation) + "/"
	locations.Archive = relative(prefix + "archive.ainovel")
	locations.Manifest = relative(prefix + "manifest.json")
	locations.Completion = relative(prefix + "completion.json")
	return locations, nil
}
func sameOrigin(a, b *url.URL) bool { return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host) }

// GenerationHrefs is a native strict bounded DAV XML parser. The source service
// used regex; this parser intentionally rejects malformed nesting, foreign XML
// namespaces, duplicate/nested href, comments containing markup tricks, CDATA,
// DTD/entities and trailing roots. XML is data and never dereferenced.
func GenerationHrefs(data []byte, root *url.URL, maximum int64) ([]string, error) {
	if root == nil || maximum <= 0 || int64(len(data)) > maximum {
		return nil, ResponseTooLarge
	}
	if !utf8.Valid(data) || bytes.Contains(data, []byte("<![CDATA[")) {
		return nil, DAVResponseInvalid
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	depth := 0
	roots := 0
	responseDepth := 0
	hrefDepth := 0
	hrefCount := 0
	responses := 0
	var href strings.Builder
	result := []string{}
	seen := map[string]bool{}
	rootPath := root.Path
	if !strings.HasSuffix(rootPath, "/") {
		rootPath += "/"
	}
	accept := func(raw string) error {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "//") || controls(raw) || strings.Contains(raw, "\\") {
			return DAVResponseInvalid
		}
		ref, err := url.Parse(raw)
		if err != nil {
			return DAVResponseInvalid
		}
		candidate := root.ResolveReference(ref)
		if !sameOrigin(candidate, root) || candidate.User != nil || candidate.RawQuery != "" || candidate.ForceQuery || candidate.Fragment != "" || strings.ContainsAny(raw, "?#") {
			return DAVResponseInvalid
		}
		decoded := candidate.Path
		if controls(decoded) || strings.Contains(decoded, "\\") {
			return DAVResponseInvalid
		}
		// ResolveReference normalizes literal .., but encoded traversal must also be
		// rejected before child extraction; encoded slash cannot introduce children.
		if decoded == rootPath || decoded == strings.TrimSuffix(rootPath, "/") {
			return nil
		}
		if !strings.HasPrefix(decoded, rootPath) {
			return DAVResponseInvalid
		}
		relative := strings.TrimSuffix(strings.TrimPrefix(decoded, rootPath), "/")
		if !uuidPattern.MatchString(relative) || strings.Contains(relative, "/") {
			return DAVResponseInvalid
		}
		if !seen[relative] {
			seen[relative] = true
			result = append(result, relative)
		}
		return nil
	}
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, DAVResponseInvalid
		}
		switch t := token.(type) {
		case xml.Directive:
			return nil, DAVResponseInvalid
		case xml.ProcInst:
			if t.Target != "xml" || depth != 0 || roots != 0 {
				return nil, DAVResponseInvalid
			}
		case xml.StartElement:
			depth++
			if depth > 32 {
				return nil, DAVResponseInvalid
			}
			if depth == 1 {
				roots++
				if roots != 1 || t.Name.Local != "multistatus" || t.Name.Space != "DAV:" {
					return nil, DAVResponseInvalid
				}
			}
			if hrefDepth != 0 {
				return nil, DAVResponseInvalid
			}
			if t.Name.Local == "response" {
				if t.Name.Space != "DAV:" || depth != 2 || responseDepth != 0 {
					return nil, DAVResponseInvalid
				}
				responseDepth = depth
				responses++
				hrefCount = 0
				if responses > 10000 {
					return nil, ResponseTooLarge
				}
			}
			if t.Name.Local == "href" {
				if t.Name.Space != "DAV:" || responseDepth == 0 || depth != responseDepth+1 {
					return nil, DAVResponseInvalid
				}
				hrefCount++
				if hrefCount > 1 {
					return nil, DAVResponseInvalid
				}
				hrefDepth = depth
				href.Reset()
			}
		case xml.CharData:
			if hrefDepth != 0 {
				href.Write(t)
			} else if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return nil, DAVResponseInvalid
			}
		case xml.EndElement:
			if depth == hrefDepth {
				if err := accept(href.String()); err != nil {
					return nil, err
				}
				hrefDepth = 0
			}
			if depth == responseDepth {
				if hrefCount != 1 {
					return nil, DAVResponseInvalid
				}
				responseDepth = 0
			}
			depth--
		}
	}
	if depth != 0 || roots != 1 || responses == 0 {
		return nil, DAVResponseInvalid
	}
	return result, nil
}
