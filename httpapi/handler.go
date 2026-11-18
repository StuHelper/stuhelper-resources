// Package httpapi translates bounded resource requests and stable errors.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	resources "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/contracts"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Authenticator interface {
	Authenticate(*http.Request) (resources.Principal, error)
}
type handler struct {
	s        *resources.Service
	auth     Authenticator
	contract routers.Router
}
type endpoint func(http.ResponseWriter, *http.Request, resources.Principal) (any, error)

func New(s *resources.Service, auth Authenticator) (http.Handler, error) {
	if s == nil || auth == nil {
		return nil, resources.ErrInput
	}
	doc, e := openapi3.NewLoader().LoadFromData(contracts.Document())
	if e != nil {
		return nil, resources.ErrInput
	}
	router, e := legacy.NewRouter(doc)
	if e != nil {
		return nil, resources.ErrInput
	}
	h := &handler{s: s, auth: auth, contract: router}
	m := http.NewServeMux()
	h.mount(m, "GET /api/v1/resources/me/summary", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		if len(r.URL.Query()) != 0 {
			return nil, resources.ErrInput
		}
		return s.OwnSummary(r.Context(), p)
	})
	h.mount(m, "GET /api/v1/resources/duplicates", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		q := r.URL.Query()
		for k := range q {
			if k != "school_id" && k != "course_id" && k != "sha256" {
				return nil, resources.ErrInput
			}
		}
		return s.FindDuplicates(r.Context(), p, q.Get("school_id"), q.Get("course_id"), q.Get("sha256"))
	})
	h.mount(m, "GET /api/v1/resources/counts", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		f, err := filter(r)
		if err != nil {
			return nil, err
		}
		var ids []string
		if r.URL.Query().Has("course_ids") {
			ids = strings.Split(r.URL.Query().Get("course_ids"), ",")
		}
		return s.Counts(r.Context(), p, f, ids)
	})
	h.mount(m, "GET /api/v1/resources/coverage", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		n, e := limit(r)
		if e != nil {
			return nil, e
		}
		return s.ListCoverage(r.Context(), p, n, r.URL.Query().Get("after"))
	})
	h.mount(m, "GET /api/v1/resources/coverage/{id}", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		return s.GetCoverage(r.Context(), p, r.PathValue("id"))
	})
	write[resources.Coverage](h, m, "PUT /api/v1/resources/coverage/{id}", func(r *http.Request, p resources.Principal, in resources.Coverage) (any, error) {
		return s.PutCoverage(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	h.mount(m, "GET /api/v1/resources/me/references", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		return s.MyReferences(r.Context(), p, strings.Split(r.URL.Query().Get("ids"), ","))
	})
	h.mount(m, "GET /api/v1/resources/capabilities", func(_ http.ResponseWriter, _ *http.Request, _ resources.Principal) (any, error) {
		return s.Capabilities(), nil
	})
	h.mount(m, "GET /api/v1/resources/categories", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		n, e := limit(r)
		if e != nil {
			return nil, e
		}
		disabled, e := boolean(r, "include_disabled")
		if e != nil {
			return nil, e
		}
		return s.ReadCategories(r.Context(), p, n, r.URL.Query().Get("after"), disabled)
	})
	write[resources.CategoryWrite](h, m, "PUT /api/v1/resources/categories/{id}", func(r *http.Request, p resources.Principal, in resources.CategoryWrite) (any, error) {
		return s.PutCategory(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	for _, route := range []struct {
		path        string
		mine, admin bool
	}{{"GET /api/v1/resources", false, false}, {"GET /api/v1/resources/me", true, false}, {"GET /api/v1/resources/manage", false, true}} {
		h.mount(m, route.path, func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
			f, e := filter(r)
			if e != nil {
				return nil, e
			}
			return s.List(r.Context(), p, f, route.mine, route.admin)
		})
	}
	h.mount(m, "GET /api/v1/resources/me/entitlements", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		f, e := filter(r)
		if e != nil {
			return nil, e
		}
		return s.ListEntitled(r.Context(), p, f)
	})
	write[resources.DraftWrite](h, m, "POST /api/v1/resources", func(r *http.Request, p resources.Principal, in resources.DraftWrite) (any, error) {
		return s.CreateDraft(r.Context(), p, key(r), in)
	})
	h.mount(m, "GET /api/v1/resources/review/{version}", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		return s.ReviewResource(r.Context(), p, r.PathValue("version"))
	})
	h.mountStream(m, "GET /api/v1/resources/review/{version}/file", h.reviewDownload)
	h.mount(m, "GET /api/v1/resources/items/{id}", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		return s.Get(r.Context(), p, r.PathValue("id"))
	})
	write[resources.DraftWrite](h, m, "PUT /api/v1/resources/items/{id}", func(r *http.Request, p resources.Principal, in resources.DraftWrite) (any, error) {
		return s.UpdateDraft(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	h.mount(m, "DELETE /api/v1/resources/items/{id}", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		q := r.URL.Query()
		var rev int64
		if len(q) > 1 || len(q) == 1 && !q.Has("revision") {
			return nil, resources.ErrInput
		}
		if raw := q.Get("revision"); raw != "" {
			n, e := strconv.ParseInt(raw, 10, 64)
			if e != nil || n < 1 {
				return nil, resources.ErrInput
			}
			rev = n
		}
		return s.DeleteDraft(r.Context(), p, r.PathValue("id"), key(r), rev)
	})
	write[resources.FileLink](h, m, "POST /api/v1/resources/items/{id}/file", func(r *http.Request, p resources.Principal, in resources.FileLink) (any, error) {
		return s.AttachFile(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	write[resources.Command](h, m, "POST /api/v1/resources/items/{id}/submit", func(r *http.Request, p resources.Principal, in resources.Command) (any, error) {
		return s.Submit(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	write[resources.Command](h, m, "POST /api/v1/resources/items/{id}/withdraw", func(r *http.Request, p resources.Principal, in resources.Command) (any, error) {
		return s.WithdrawProposal(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	write[resources.Command](h, m, "POST /api/v1/resources/items/{id}/retry", func(r *http.Request, p resources.Principal, in resources.Command) (any, error) {
		return s.RetryPending(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	write[resources.Decision](h, m, "POST /api/v1/resources/items/{id}/decisions", func(r *http.Request, p resources.Principal, in resources.Decision) (any, error) {
		return s.ApplyDecision(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	write[resources.StateChange](h, m, "POST /api/v1/resources/items/{id}/state", func(r *http.Request, p resources.Principal, in resources.StateChange) (any, error) {
		return s.SetState(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	h.mount(m, "GET /api/v1/resources/requests", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		n, e := limit(r)
		if e != nil {
			return nil, e
		}
		return s.Requests(r.Context(), p, n, r.URL.Query().Get("after"))
	})
	h.mount(m, "GET /api/v1/resources/requests/{id}", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		return s.RequestStatus(r.Context(), p, r.PathValue("id"))
	})
	h.mount(m, "POST /api/v1/resources/requests/{id}/retry", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		var q struct{}
		if e := decode(r, &q); e != nil {
			return nil, e
		}
		return s.RetryRequest(r.Context(), p, r.PathValue("id"), key(r))
	})
	write[resources.Request](h, m, "POST /api/v1/resources/items/{id}/requests", func(r *http.Request, p resources.Principal, in resources.Request) (any, error) {
		return s.CreateRequest(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	h.mount(m, "GET /api/v1/resources/items/{id}/grants", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		n, e := limit(r)
		if e != nil {
			return nil, e
		}
		return s.Grants(r.Context(), p, r.PathValue("id"), n, r.URL.Query().Get("after"))
	})
	write[resources.GrantWrite](h, m, "PUT /api/v1/resources/items/{id}/grants/{grant}", func(r *http.Request, p resources.Principal, in resources.GrantWrite) (any, error) {
		return s.PutGrant(r.Context(), p, r.PathValue("id"), r.PathValue("grant"), key(r), in)
	})
	h.mount(m, "GET /api/v1/resources/directories", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		tree, e := boolean(r, "tree")
		if e != nil {
			return nil, e
		}
		q := r.URL.Query()
		if tree {
			if q.Get("parent_id") != "" || q.Get("after") != "" || q.Has("limit") {
				return nil, resources.ErrInput
			}
			return s.CourseDirectoryTree(r.Context(), p, q.Get("school_id"), q.Get("course_id"))
		}
		n, e := limit(r)
		if e != nil {
			return nil, e
		}
		return s.Directories(r.Context(), p, q.Get("school_id"), q.Get("course_id"), q.Get("parent_id"), n, q.Get("after"))
	})
	write[resources.DirectoryWrite](h, m, "PUT /api/v1/resources/directories/{id}", func(r *http.Request, p resources.Principal, in resources.DirectoryWrite) (any, error) {
		return s.PutDirectory(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	write[resources.PackageRequest](h, m, "POST /api/v1/resources/packages", func(r *http.Request, p resources.Principal, in resources.PackageRequest) (any, error) {
		return s.CreatePackage(r.Context(), p, key(r), in)
	})
	h.mount(m, "GET /api/v1/resources/packages/{id}", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		out, e := s.GetPackage(r.Context(), p, r.PathValue("id"))
		return out.Package, e
	})
	h.mountStream(m, "GET /api/v1/resources/items/{id}/download", h.download)
	h.mountStream(m, "GET /api/v1/resources/items/{id}/preview", h.preview)
	h.mountStream(m, "GET /api/v1/resources/packages/{id}/parts/{part}", h.downloadPackage)
	return m, nil
}
func key(r *http.Request) string { return r.Header.Get("Idempotency-Key") }
func write[T any](h *handler, m *http.ServeMux, path string, run func(*http.Request, resources.Principal, T) (any, error)) {
	h.mount(m, path, func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		var in T
		if e := decode(r, &in); e != nil {
			return nil, e
		}
		return run(r, p, in)
	})
}
func (h *handler) mount(m *http.ServeMux, path string, run endpoint) {
	h.mountWithMode(m, path, false, run)
}
func (h *handler) mountStream(m *http.ServeMux, path string, run endpoint) {
	h.mountWithMode(m, path, true, run)
}

// Streaming preflight has a fixed deadline. Once bytes start flowing, each
// socket write gets its own idle deadline and the service monitors upstream progress.
type streamResponseWriter struct {
	http.ResponseWriter
	preflight *time.Timer
	timeout   time.Duration
}

func (w *streamResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *streamResponseWriter) Write(p []byte) (int, error) {
	w.preflight.Stop()
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.timeout))
	return w.ResponseWriter.Write(p)
}

func (h *handler) mountWithMode(m *http.ServeMux, path string, streaming bool, run endpoint) {
	m.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		var ctx context.Context
		var cancel context.CancelFunc
		if streaming {
			ctx, cancel = context.WithCancel(r.Context())
			timeout := h.s.OperationTimeout()
			preflight := time.AfterFunc(timeout, cancel)
			defer preflight.Stop()
			w = &streamResponseWriter{ResponseWriter: w, preflight: preflight, timeout: timeout}
		} else {
			ctx, cancel = context.WithTimeout(r.Context(), 2*time.Minute)
		}
		defer cancel()
		r = r.WithContext(ctx)
		controller := http.NewResponseController(w)
		initialDeadline := time.Now().Add(2 * time.Minute)
		if deadline, ok := ctx.Deadline(); ok && deadline.Before(initialDeadline) {
			initialDeadline = deadline
		}
		_ = controller.SetReadDeadline(initialDeadline)
		_ = controller.SetWriteDeadline(initialDeadline)
		stopped := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			_ = controller.SetReadDeadline(time.Now())
			if !streaming {
				_ = controller.SetWriteDeadline(time.Now())
			}
			close(stopped)
		})
		defer func() {
			if !stop() {
				<-stopped
			}
			_ = controller.SetReadDeadline(time.Time{})
			_ = controller.SetWriteDeadline(time.Time{})
		}()
		p, e := h.auth.Authenticate(r)
		if e != nil || !p.Valid() {
			if errors.Is(e, resources.ErrUnavailable) {
				failure(w, e)
			} else {
				failure(w, resources.ErrUnauthenticated)
			}
			return
		}

		var bodyLimit int64 = 128 << 10
		if path == "POST /api/v1/resources/packages" {
			// A compact request can contain 10,000 identifiers of up to 128 ASCII bytes.
			bodyLimit = 2 << 20
		}
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
		if len(r.Header.Values("Idempotency-Key")) > 1 {
			failure(w, resources.ErrInput)
			return
		}
		for _, values := range r.URL.Query() {
			if len(values) != 1 {
				failure(w, resources.ErrInput)
				return
			}
		}
		route, params, err := h.contract.FindRoute(r)
		if err != nil {
			failure(w, resources.ErrInput)
			return
		}
		// Check absence here, including chunked requests. The validator's
		// RejectWhenRequestBodyNotSpecified flag also rejects specified bodies
		// in the pinned version, so only schema validation is delegated below.
		if route.Operation.RequestBody == nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1))
			if err != nil || len(body) != 0 {
				failure(w, resources.ErrInput)
				return
			}
		}
		// Authentication already succeeded above. This validates the contract's
		// required fields, nested shapes and limits against the same request body.
		if err = openapi3filter.ValidateRequest(ctx, &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route, Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, SkipSettingDefaults: true}}); err != nil {
			failure(w, resources.ErrInput)
			return
		}
		out, e := run(w, r, p)
		if e != nil {
			failure(w, e)
			return
		}
		if out != nil {
			codes := []int{}
			for code := range route.Operation.Responses.Map() {
				n, e := strconv.Atoi(code)
				if e == nil && n >= 200 && n < 300 {
					codes = append(codes, n)
				}
			}
			slices.Sort(codes)
			status := 200
			if len(codes) > 0 {
				status = codes[0]
			}
			writeJSON(w, status, out)
		}
	})
}
func decode(r *http.Request, out any) error {
	kind, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || kind != "application/json" {
		return resources.ErrInput
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return resources.ErrInput
	}
	return nil
}
func limit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 25, nil
	}
	n, e := strconv.Atoi(raw)
	if e != nil || n < 1 || n > 100 {
		return 0, resources.ErrInput
	}
	return n, nil
}
func boolean(r *http.Request, key string) (bool, error) {
	switch r.URL.Query().Get(key) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, resources.ErrInput
	}
}
func filter(r *http.Request) (resources.Filter, error) {
	q := r.URL.Query()
	n, e := limit(r)
	failedOnly, fe := boolean(r, "failed_only")
	out := resources.Filter{SchoolID: q.Get("school_id"), CourseID: q.Get("course_id"), CategoryID: q.Get("category_id"), DirectoryID: q.Get("directory_id"), TeacherID: q.Get("teacher_id"), TermID: q.Get("term_id"), Tag: q.Get("tag"), Access: q.Get("access"), State: q.Get("state"), ProposalState: q.Get("proposal_state"), FailedOnly: failedOnly, Query: q.Get("query"), After: q.Get("after"), Limit: n, Sort: q.Get("sort"), FileType: q.Get("file_type")}
	if e != nil || fe != nil {
		return out, resources.ErrInput
	}
	if raw := q.Get("attributes"); raw != "" {
		if !utf8.ValidString(raw) || utf8.RuneCountInString(raw) > 2000 {
			return out, resources.ErrInput
		}
		d := json.NewDecoder(strings.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&out.Attributes) != nil || d.Decode(new(any)) != io.EOF || out.Attributes == nil {
			return out, resources.ErrInput
		}
	}
	return out, nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Del("Content-Disposition")
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, e error) {
	status, code := 503, resources.ErrUnavailable.Error()
	for _, item := range []struct {
		err    error
		status int
	}{{resources.ErrRateLimited, 429}, {resources.ErrInput, 400}, {resources.ErrUnauthenticated, 401}, {resources.ErrDenied, 403}, {resources.ErrNotFound, 404}, {resources.ErrConflict, 409}, {resources.ErrCapacity, 413}} {
		if errors.Is(e, item.err) {
			status, code = item.status, item.err.Error()
			break
		}
	}
	payload := map[string]any{"code": code}
	var detail *resources.AccessFailure
	if errors.As(e, &detail) {
		payload["access_reason"] = detail.Reason
		if len(detail.Missing) > 0 {
			payload["missing_conditions"] = detail.Missing
		}
		if len(detail.Unavailable) > 0 {
			payload["unavailable_conditions"] = detail.Unavailable
		}
		if detail.RuleVersion != "" {
			payload["rule_version"] = detail.RuleVersion
		}
	}
	writeJSON(w, status, payload)
}

type stream struct {
	w             http.ResponseWriter
	kind, name    string
	etag          string
	contentRange  string
	contentLength int64
	bytes         int64
	status        int
	inline        bool
	written       bool
}

func formatContentDisposition(disposition, name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned != "" && cleaned != "." && cleaned != ".." {
		if formatted := mime.FormatMediaType(disposition, map[string]string{"filename": cleaned}); formatted != "" {
			return formatted
		}
	}
	return disposition
}

func (s *stream) Write(b []byte) (int, error) {
	if !s.written {
		s.w.Header().Set("Content-Type", s.kind)
		disposition := "attachment"
		if s.inline {
			disposition = "inline"
			s.w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
		}
		s.w.Header().Set("Content-Disposition", formatContentDisposition(disposition, s.name))
		if s.etag != "" {
			s.w.Header().Set("ETag", s.etag)
			s.w.Header().Set("Accept-Ranges", "bytes")
		}
		if s.contentLength > 0 {
			s.w.Header().Set("Content-Length", strconv.FormatInt(s.contentLength, 10))
		}
		if s.contentRange != "" {
			s.w.Header().Set("Content-Range", s.contentRange)
		}
		if s.status != 0 && s.status != http.StatusOK {
			s.w.WriteHeader(s.status)
		}
		s.written = true
	}
	n, err := s.w.Write(b)
	s.bytes += int64(n)
	return n, err
}

func etagMatches(headerVal, sha256 string) bool {
	raw := strings.TrimSpace(headerVal)
	if raw == "" {
		return false
	}
	if raw == "*" {
		return true
	}
	target := "\"" + sha256 + "\""
	for _, part := range strings.Split(raw, ",") {
		tag := strings.TrimSpace(part)
		tag = strings.TrimPrefix(tag, "W/")
		if tag == target {
			return true
		}
	}
	return false
}

func parseByteRange(rangeHeader, ifRange, etag string, size int64) (*resources.ByteRange, bool) {
	if rangeHeader == "" {
		return nil, true
	}
	if cond := strings.TrimSpace(ifRange); cond != "" && cond != etag {
		return nil, true
	}
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return nil, true
	}
	spec := strings.TrimSpace(strings.TrimPrefix(rangeHeader, "bytes="))
	if spec == "" || size <= 0 {
		return nil, false
	}
	if strings.Contains(spec, ",") {
		return nil, true
	}
	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok || startStr == "" && endStr == "" {
		return nil, false
	}
	if startStr == "" {
		if strings.HasPrefix(endStr, "+") || strings.HasPrefix(endStr, "-") {
			return nil, false
		}
		suffix, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || suffix <= 0 {
			return nil, false
		}
		if suffix >= size {
			return &resources.ByteRange{Offset: 0, Length: size}, true
		} else {
			return &resources.ByteRange{Offset: size - suffix, Length: suffix}, true
		}
	}
	if strings.HasPrefix(startStr, "+") || strings.HasPrefix(startStr, "-") {
		return nil, false
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 || start >= size {
		return nil, false
	}
	if endStr == "" {
		return &resources.ByteRange{Offset: start, Length: size - start}, true
	}
	if strings.HasPrefix(endStr, "+") || strings.HasPrefix(endStr, "-") {
		return nil, false
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return nil, false
	}
	if end >= size {
		end = size - 1
	}
	return &resources.ByteRange{Offset: start, Length: end - start + 1}, true
}

func (h *handler) download(w http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
	return h.file(w, r, p, false)
}
func (h *handler) preview(w http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
	return h.file(w, r, p, true)
}
func (h *handler) file(w http.ResponseWriter, r *http.Request, p resources.Principal, inline bool) (any, error) {
	var out *stream
	e := h.s.StreamDownloadRange(r.Context(), p, r.PathValue("id"), func(meta resources.File) (io.Writer, *resources.ByteRange) {
		etag := "\"" + meta.SHA256 + "\""
		if etagMatches(r.Header.Get("If-None-Match"), meta.SHA256) {
			w.Header().Set("ETag", etag)
			w.Header().Set("Accept-Ranges", "bytes")
			if inline {
				w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
			}
			w.WriteHeader(http.StatusNotModified)
			return nil, nil
		}
		br, satisfiable := parseByteRange(r.Header.Get("Range"), r.Header.Get("If-Range"), etag, meta.Size)
		if !satisfiable {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(meta.Size, 10))
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("ETag", etag)
			writeJSON(w, http.StatusRequestedRangeNotSatisfiable, map[string]any{"code": resources.ErrInput.Error()})
			return nil, nil
		}
		kind := "application/octet-stream"
		if inline {
			kind = meta.ContentType
		}

		out = &stream{w: w, kind: kind, name: meta.Filename, inline: inline, etag: etag, contentLength: meta.Size, status: http.StatusOK}
		if br != nil {
			out.status = http.StatusPartialContent
			out.contentLength = br.Length
			out.contentRange = fmt.Sprintf("bytes %d-%d/%d", br.Offset, br.Offset+br.Length-1, meta.Size)
		}
		return out, br
	})
	if out != nil {
		if out.written && (e != nil || out.bytes != out.contentLength) {
			panic(http.ErrAbortHandler)
		}
		if e == nil && out.bytes != out.contentLength {
			return nil, resources.ErrUnavailable
		}
	}
	return nil, e
}
func (h *handler) downloadPackage(w http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
	part, e := strconv.Atoi(r.PathValue("part"))
	if e != nil {
		return nil, resources.ErrInput
	}
	out := &stream{w: w, kind: "application/zip", name: "resources-part-" + strconv.Itoa(part) + ".zip"}
	e = h.s.DownloadPackage(r.Context(), p, r.PathValue("id"), part, out)
	if e != nil && out.written {
		panic(http.ErrAbortHandler)
	}
	return nil, e
}

func (h *handler) reviewDownload(w http.ResponseWriter, req *http.Request, p resources.Principal) (any, error) {
	entry, e := h.s.ReviewResource(req.Context(), p, req.PathValue("version"))
	if e != nil {
		return nil, e
	}
	version := entry.Proposal
	if version == nil || version.ID != req.PathValue("version") {
		version = entry.Current
	}
	if version == nil || version.File == nil {
		return nil, resources.ErrNotFound
	}
	writer := &stream{w: w, kind: "application/octet-stream", name: version.File.Filename}
	e = h.s.DownloadReviewFile(req.Context(), p, version.ID, writer)
	if e != nil && writer.written {
		panic(http.ErrAbortHandler)
	}
	return nil, e
}
