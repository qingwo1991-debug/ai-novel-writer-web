package webdav

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Dispatch must be a deployment-owned, redirect-disabled HTTP transport. It must
// enforce pinned/validated DNS and exact-origin policy, including proxy policy.
// There is deliberately no default transport, http.Client or global fallback.
type Dispatch func(*http.Request) (*http.Response, error)
type Authorize func(context.Context, *url.URL) error
type Options struct {
	HTTPDispatch Dispatch
	// Authorize is mandatory and called on an isolated URL snapshot EVERY request.
	// It must enforce deployment origin + DNS/SSRF policy; account input grants no
	// network authority. The callback cannot change the actual dispatched URL.
	Authorize           Authorize
	GenerationIDFactory func() string
	Now                 func() time.Time
	Timeout             time.Duration
	TransferTimeout     time.Duration
	MaxXMLBytes         int64
	MaxJSONBytes        int64
	MaxArchiveBytes     int64
}

// Account lives only in caller memory and per-operation request headers. Service
// does not retain it, log it, persist it or return it in any result/error.
type Account struct{ Endpoint, Username, Secret string }
type AppendInput struct {
	Account                    Account
	CloudBookID                string
	Archive                    io.Reader
	ArchiveSHA256              string
	ArchiveByteSize            int64
	OriginProjectID            string
	PortableSnapshotGeneration string
	ParentGenerationIDs        []string
}
type Service struct{ options Options }

func New(o Options) (*Service, error) {
	if o.HTTPDispatch == nil || o.Authorize == nil {
		return nil, InputInvalid
	}
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	if o.TransferTimeout <= 0 {
		o.TransferTimeout = 30 * time.Minute
	}
	if o.MaxXMLBytes <= 0 {
		o.MaxXMLBytes = 1 << 20
	}
	if o.MaxJSONBytes <= 0 {
		o.MaxJSONBytes = 1 << 20
	}
	if o.MaxArchiveBytes <= 0 {
		o.MaxArchiveBytes = 16 << 30
	}
	if o.MaxXMLBytes > 16<<20 || o.MaxJSONBytes > 16<<20 || o.MaxArchiveBytes > maxSafeInteger {
		return nil, InputInvalid
	}
	if o.GenerationIDFactory == nil {
		o.GenerationIDFactory = randomID
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o}, nil
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func accountEndpoint(a Account) (*url.URL, error) {
	if a.Username == "" || a.Secret == "" || len(a.Username) > 4096 || len(a.Secret) > 65536 || !utf8.ValidString(a.Username) || !utf8.ValidString(a.Secret) || controls(a.Username) || controls(a.Secret) {
		return nil, InputInvalid
	}
	normalized, err := NormalizeEndpoint(a.Endpoint)
	if err != nil {
		return nil, InputInvalid
	}
	return url.Parse(normalized)
}
func operationError(ctx context.Context, fallback Error) error {
	if ctx.Err() != nil {
		return Cancelled
	}
	return fallback
}
func statusError(status int) error {
	if status == 401 || status == 403 {
		return Auth
	}
	if status == 404 {
		return NotFound
	}
	return Network
}

type responseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *responseBody) Close() error { _ = b.ReadCloser.Close(); b.cancel(); return nil }
func (s *Service) request(ctx context.Context, a Account, target *url.URL, method string, body io.Reader, length int64, transfer bool) (*http.Response, error) {
	endpoint, err := accountEndpoint(a)
	if err != nil {
		return nil, err
	}
	if ctx == nil || target == nil {
		return nil, InputInvalid
	}
	if ctx.Err() != nil {
		return nil, Cancelled
	}
	captured := *target
	if !sameOrigin(&captured, endpoint) || captured.User != nil || captured.RawQuery != "" || captured.Fragment != "" || !strings.HasPrefix(captured.EscapedPath(), endpoint.EscapedPath()) {
		return nil, InputInvalid
	}
	timeout := s.options.Timeout
	if transfer {
		timeout = s.options.TransferTimeout
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	authorizationTarget := captured
	if err := s.options.Authorize(requestContext, &authorizationTarget); err != nil {
		cancel()
		return nil, operationError(ctx, Network)
	}
	if requestContext.Err() != nil {
		cancel()
		return nil, operationError(ctx, Network)
	}
	req, err := http.NewRequestWithContext(requestContext, method, captured.String(), body)
	if err != nil {
		cancel()
		return nil, InputInvalid
	}
	req.SetBasicAuth(a.Username, a.Secret)
	req.Header.Set("Accept-Encoding", "identity")
	if method == "PROPFIND" {
		req.Header.Set("Accept", "application/xml")
		req.Header.Set("Depth", "1")
		if captured.String() == endpoint.String() {
			req.Header.Set("Depth", "0")
		}
	}
	if method == "MKCOL" || method == "PUT" {
		req.Header.Set("If-None-Match", "*")
	}
	if method == "PUT" {
		req.ContentLength = length
		req.Header.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	response, err := s.options.HTTPDispatch(req)
	if err != nil || response == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		cancel()
		return nil, operationError(ctx, Network)
	}
	if response.Body == nil {
		response.Body = io.NopCloser(bytes.NewReader(nil))
	}
	response.Body = &responseBody{response.Body, cancel}
	// Do not accept custom dispatchers disguising a followed redirect as 200.
	if response.StatusCode >= 300 && response.StatusCode < 400 || response.Request != nil && response.Request.URL != nil && response.Request.URL.String() != captured.String() {
		_ = response.Body.Close()
		return nil, Network
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		_ = response.Body.Close()
		return nil, RemoteInvalid
	}
	if ctx.Err() != nil {
		_ = response.Body.Close()
		return nil, Cancelled
	}
	return response, nil
}
func contentLength(r *http.Response) (int64, error) {
	values := r.Header.Values("Content-Length")
	if len(values) > 1 {
		return 0, RemoteInvalid
	}
	if len(values) == 1 {
		raw := values[0]
		if raw == "" || strings.Trim(raw, "0123456789") != "" {
			return 0, RemoteInvalid
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 || n > maxSafeInteger || r.ContentLength >= 0 && r.ContentLength != n {
			return 0, RemoteInvalid
		}
		return n, nil
	}
	if r.ContentLength < -1 || r.ContentLength > maxSafeInteger {
		return 0, RemoteInvalid
	}
	return r.ContentLength, nil
}
func readResponse(ctx context.Context, r *http.Response, maximum int64) ([]byte, error) {
	defer r.Body.Close()
	declared, err := contentLength(r)
	if err != nil {
		return nil, err
	}
	if declared > maximum {
		return nil, ResponseTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(&contextReader{ctx, r.Body}, maximum+1))
	if err != nil {
		return nil, operationError(ctx, Network)
	}
	if int64(len(b)) > maximum {
		return nil, ResponseTooLarge
	}
	if declared >= 0 && int64(len(b)) != declared {
		return nil, RemoteInvalid
	}
	return b, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, Cancelled
	}
	n, err := r.reader.Read(p)
	if r.ctx.Err() != nil {
		return n, Cancelled
	}
	return n, err
}
func digestBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// verifyingReader exposes untrusted streaming bytes internally only. EOF (and
// Close returning nil) is the integrity boundary. A consumer MUST stage bytes
// until EOF and a nil Close, never publish partial bytes or automatically restore.
// Closing early is an integrity error, not successful verification.
type verifyingReader struct {
	ctx      context.Context
	body     io.ReadCloser
	hash     hash.Hash
	expected ArchiveDescriptor
	maximum  int64
	count    int64
	verified bool
	failed   error
	closed   bool
}

func newVerifier(ctx context.Context, body io.ReadCloser, a ArchiveDescriptor, maximum int64) *verifyingReader {
	return &verifyingReader{ctx: ctx, body: body, hash: sha256.New(), expected: a, maximum: maximum}
}
func (r *verifyingReader) Read(p []byte) (int, error) {
	if r.failed != nil {
		return 0, r.failed
	}
	if r.closed {
		return 0, RemoteInvalid
	}
	if r.verified {
		return 0, io.EOF
	}
	if r.ctx.Err() != nil {
		r.failed = Cancelled
		return 0, r.failed
	}
	if len(p) == 0 {
		return 0, nil
	}
	remaining := r.expected.ByteSize - r.count
	if int64(len(p)) > remaining+1 {
		p = p[:int(remaining+1)]
	}
	n, err := r.body.Read(p)
	if n < 0 || n > len(p) {
		r.failed = RemoteInvalid
		return 0, r.failed
	}
	r.count += int64(n)
	if r.ctx.Err() != nil {
		r.failed = Cancelled
		return 0, r.failed
	}
	if r.count > r.expected.ByteSize || r.count > r.maximum {
		r.failed = RemoteInvalid
		return 0, r.failed
	}
	_, _ = r.hash.Write(p[:n])
	if err == io.EOF {
		if r.count != r.expected.ByteSize || hex.EncodeToString(r.hash.Sum(nil)) != r.expected.SHA256 {
			r.failed = RemoteInvalid
			return n, r.failed
		}
		r.verified = true
		return n, io.EOF
	}
	if err != nil {
		r.failed = Network
		return n, r.failed
	}
	return n, nil
}
func (r *verifyingReader) Close() error {
	if !r.closed {
		_ = r.body.Close()
		r.closed = true
	}
	if r.ctx.Err() != nil {
		return Cancelled
	}
	if r.failed != nil {
		return r.failed
	}
	if !r.verified {
		return RemoteInvalid
	}
	return nil
}
func verifyResponse(ctx context.Context, r *http.Response, a ArchiveDescriptor, maximum int64) error {
	defer r.Body.Close()
	declared, err := contentLength(r)
	if err != nil {
		return err
	}
	if declared >= 0 && declared != a.ByteSize {
		return RemoteInvalid
	}
	if a.ByteSize > maximum {
		return ResponseTooLarge
	}
	reader := newVerifier(ctx, r.Body, a, maximum)
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		return err
	}
	return reader.Close()
}
func (s *Service) CheckConnection(ctx context.Context, a Account) error {
	endpoint, err := accountEndpoint(a)
	if err != nil {
		return err
	}
	r, err := s.request(ctx, a, endpoint, "PROPFIND", nil, 0, false)
	if err != nil {
		return err
	}
	if r.StatusCode != 207 && (r.StatusCode < 200 || r.StatusCode >= 300) {
		_ = r.Body.Close()
		return statusError(r.StatusCode)
	}
	_, err = readResponse(ctx, r, s.options.MaxXMLBytes)
	return err
}
func (s *Service) existingMatches(ctx context.Context, a Account, u *url.URL, expected ArchiveDescriptor, allowMissing, transfer bool) (bool, error) {
	r, err := s.request(ctx, a, u, "GET", nil, 0, transfer)
	if err != nil {
		return false, err
	}
	if r.StatusCode == 404 && allowMissing {
		_ = r.Body.Close()
		return false, nil
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		_ = r.Body.Close()
		return false, statusError(r.StatusCode)
	}
	if err := verifyResponse(ctx, r, expected, s.options.MaxArchiveBytes); err != nil {
		if allowMissing && (errors.Is(err, RemoteInvalid) || errors.Is(err, ResponseTooLarge)) {
			return false, RemoteConflict
		}
		return false, err
	}
	return true, nil
}

// put streams exactly once and reconciles an uncertain PUT by GET, never by a
// blind overwrite/retry. All objects, including completion, use If-None-Match:*.
func (s *Service) put(ctx context.Context, a Account, u *url.URL, reader io.Reader, expected ArchiveDescriptor, transfer bool) error {
	matched, err := s.existingMatches(ctx, a, u, expected, true, transfer)
	if err != nil {
		return err
	}
	local := newVerifier(ctx, io.NopCloser(reader), expected, s.options.MaxArchiveBytes)
	if matched {
		_, err = io.Copy(io.Discard, local)
		if err != nil {
			return err
		}
		return local.Close()
	}
	r, putErr := s.request(ctx, a, u, "PUT", io.NopCloser(local), expected.ByteSize, transfer)
	uncertain := putErr != nil
	if putErr != nil && putErr != Network {
		return putErr
	}
	if r != nil {
		status := r.StatusCode
		_ = r.Body.Close()
		if status < 200 || status >= 300 {
			if status == 409 || status == 412 || status == 408 || status >= 500 {
				uncertain = true
			} else {
				return statusError(status)
			}
		}
	}
	if ctx.Err() != nil {
		return Cancelled
	}
	// net/http may consume exactly Content-Length without requesting EOF. Confirm
	// the source's final byte count/hash and reject an extra byte before commit.
	if local.failed != nil {
		return local.failed
	}
	if local.count != expected.ByteSize {
		return operationError(ctx, RemoteInvalid)
	}
	if !local.verified {
		var extra [1]byte
		n, err := local.Read(extra[:])
		if n != 0 || err != io.EOF {
			if err != nil {
				return err
			}
			return RemoteInvalid
		}
	}
	if err := local.Close(); err != nil {
		return err
	}
	matched, err = s.existingMatches(ctx, a, u, expected, false, transfer)
	if err != nil {
		if uncertain && (err == NotFound || err == RemoteInvalid || err == ResponseTooLarge) {
			return RemoteConflict
		}
		return err
	}
	if !matched {
		if uncertain {
			return RemoteConflict
		}
		return RemoteInvalid
	}
	return nil
}
func (s *Service) putBytes(ctx context.Context, a Account, u *url.URL, b []byte) error {
	return s.put(ctx, a, u, bytes.NewReader(b), ArchiveDescriptor{SHA256: digestBytes(b), ByteSize: int64(len(b))}, false)
}
func (s *Service) AppendGeneration(ctx context.Context, input AppendInput) (Generation, error) {
	if _, err := accountEndpoint(input.Account); err != nil {
		return Generation{}, err
	}
	if input.Archive == nil || !idPattern.MatchString(input.CloudBookID) || !uuidPattern.MatchString(input.OriginProjectID) || !idPattern.MatchString(input.PortableSnapshotGeneration) || !hashPattern.MatchString(input.ArchiveSHA256) || input.ArchiveByteSize < 0 || input.ArchiveByteSize > s.options.MaxArchiveBytes || len(input.ParentGenerationIDs) > 256 {
		return Generation{}, InputInvalid
	}
	parents := []string{}
	seen := map[string]bool{}
	for _, p := range input.ParentGenerationIDs {
		if !idPattern.MatchString(p) {
			return Generation{}, InputInvalid
		}
		if !seen[p] {
			parents = append(parents, p)
			seen[p] = true
		}
	}
	id := s.options.GenerationIDFactory()
	if !uuidPattern.MatchString(id) {
		return Generation{}, InputInvalid
	}
	created := s.options.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if !validTime(created) {
		return Generation{}, InputInvalid
	}
	locations, err := GenerationLocations(input.Account.Endpoint, input.CloudBookID, id)
	if err != nil {
		return Generation{}, err
	}
	manifest := Manifest{1, input.CloudBookID, id, parents, created, input.OriginProjectID, input.PortableSnapshotGeneration, ArchiveDescriptor{"archive.ainovel", input.ArchiveSHA256, input.ArchiveByteSize}}
	manifestBytes, err := wireJSON(manifest)
	if err != nil || int64(len(manifestBytes)) > s.options.MaxJSONBytes {
		return Generation{}, InputInvalid
	}
	for _, collection := range locations.Collections {
		r, err := s.request(ctx, input.Account, collection, "MKCOL", nil, 0, false)
		if err != nil {
			return Generation{}, err
		}
		status := r.StatusCode
		_ = r.Body.Close()
		if status != 201 && status != 405 {
			return Generation{}, statusError(status)
		}
	}
	if err := s.put(ctx, input.Account, locations.Archive, input.Archive, manifest.Archive, true); err != nil {
		return Generation{}, err
	}
	if err := s.putBytes(ctx, input.Account, locations.Manifest, manifestBytes); err != nil {
		return Generation{}, err
	}
	completed := s.options.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if !validTime(completed) {
		return Generation{}, InputInvalid
	}
	completion := Completion{1, input.CloudBookID, id, ManifestDescriptor{"manifest.json", digestBytes(manifestBytes), int64(len(manifestBytes))}, manifest.Archive, completed}
	completionBytes, err := wireJSON(completion)
	if err != nil || int64(len(completionBytes)) > s.options.MaxJSONBytes {
		return Generation{}, InputInvalid
	}
	// LAST publication object. No cancellation, failed readback or local hash
	// failure in archive/manifest phases can reach this write.
	if err := s.putBytes(ctx, input.Account, locations.Completion, completionBytes); err != nil {
		return Generation{}, err
	}
	return publicGeneration(manifest, nil), nil
}
func (s *Service) inspect(ctx context.Context, a Account, book, id string) (Manifest, Locations, error) {
	locations, err := GenerationLocations(a.Endpoint, book, id)
	if err != nil || id == "" {
		return Manifest{}, Locations{}, InputInvalid
	}
	fetch := func(u *url.URL) ([]byte, error) {
		r, err := s.request(ctx, a, u, "GET", nil, 0, false)
		if err != nil {
			return nil, err
		}
		if r.StatusCode < 200 || r.StatusCode >= 300 {
			_ = r.Body.Close()
			return nil, statusError(r.StatusCode)
		}
		return readResponse(ctx, r, s.options.MaxJSONBytes)
	}
	b, err := fetch(locations.Completion)
	if err != nil {
		return Manifest{}, locations, err
	}
	completion, err := ParseCompletion(b)
	if err != nil {
		return Manifest{}, locations, err
	}
	if completion.CloudBookID != book || completion.GenerationID != id || completion.Archive.ByteSize > s.options.MaxArchiveBytes || completion.Manifest.ByteSize > s.options.MaxJSONBytes {
		return Manifest{}, locations, RemoteInvalid
	}
	b, err = fetch(locations.Manifest)
	if err != nil {
		return Manifest{}, locations, err
	}
	if int64(len(b)) != completion.Manifest.ByteSize || digestBytes(b) != completion.Manifest.SHA256 {
		return Manifest{}, locations, RemoteInvalid
	}
	manifest, err := ParseManifest(b)
	if err != nil {
		return Manifest{}, locations, err
	}
	if manifest.CloudBookID != book || manifest.GenerationID != id || manifest.Archive != completion.Archive {
		return Manifest{}, locations, RemoteInvalid
	}
	return manifest, locations, nil
}

// OpenGeneration returns internal streaming archive bytes, not a server path or
// restore capability. Callers must consume to verified EOF, check Close, stage
// independently and obtain publication authorization before using the content.
func (s *Service) OpenGeneration(ctx context.Context, a Account, book, id string) (io.ReadCloser, Generation, error) {
	manifest, locations, err := s.inspect(ctx, a, book, id)
	if err != nil {
		return nil, Generation{}, err
	}
	r, err := s.request(ctx, a, locations.Archive, "GET", nil, 0, true)
	if err != nil {
		return nil, Generation{}, err
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		_ = r.Body.Close()
		return nil, Generation{}, statusError(r.StatusCode)
	}
	declared, err := contentLength(r)
	if err != nil || declared >= 0 && declared != manifest.Archive.ByteSize {
		_ = r.Body.Close()
		return nil, Generation{}, RemoteInvalid
	}
	return newVerifier(ctx, r.Body, manifest.Archive, s.options.MaxArchiveBytes), publicGeneration(manifest, nil), nil
}
func (s *Service) ListGenerations(ctx context.Context, a Account, book string) ([]Generation, error) {
	locations, err := GenerationLocations(a.Endpoint, book, "")
	if err != nil {
		return nil, err
	}
	r, err := s.request(ctx, a, locations.Generations, "PROPFIND", nil, 0, false)
	if err != nil {
		return nil, err
	}
	if r.StatusCode == 404 {
		_ = r.Body.Close()
		return []Generation{}, nil
	}
	if r.StatusCode != 207 {
		_ = r.Body.Close()
		return nil, statusError(r.StatusCode)
	}
	b, err := readResponse(ctx, r, s.options.MaxXMLBytes)
	if err != nil {
		return nil, err
	}
	ids, err := GenerationHrefs(b, locations.Generations, s.options.MaxXMLBytes)
	if err != nil {
		return nil, err
	}
	manifests := []Manifest{}
	for _, id := range ids {
		manifest, where, err := s.inspect(ctx, a, book, id)
		if err == NotFound || err == RemoteInvalid {
			continue
		}
		if err != nil {
			return nil, err
		}
		matched, err := s.existingMatches(ctx, a, where.Archive, manifest.Archive, false, true)
		if err == NotFound || err == RemoteInvalid || err == ResponseTooLarge {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		manifests = append(manifests, manifest)
	}
	return Catalog(manifests), nil
}
