// Package httpapi translates bounded resource requests and stable errors.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	resources "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/contracts"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"
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
	h.mount(m, "GET /api/v1/resources/capabilities", func(_ http.ResponseWriter, _ *http.Request, _ resources.Principal) (any, error) {
		return s.Capabilities(), nil
	})
	h.mount(m, "GET /api/v1/resources/categories", func(_ http.ResponseWriter, r *http.Request, _ resources.Principal) (any, error) {
		n, e := limit(r)
		if e != nil {
			return nil, e
		}
		disabled, e := boolean(r, "include_disabled")
		if e != nil {
			return nil, e
		}
		return s.Categories(r.Context(), n, r.URL.Query().Get("after"), disabled)
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
	write[resources.DraftWrite](h, m, "POST /api/v1/resources", func(r *http.Request, p resources.Principal, in resources.DraftWrite) (any, error) {
		return s.CreateDraft(r.Context(), p, key(r), in)
	})
	h.mount(m, "GET /api/v1/resources/items/{id}", func(_ http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
		return s.Get(r.Context(), p, r.PathValue("id"))
	})
	write[resources.DraftWrite](h, m, "PUT /api/v1/resources/items/{id}", func(r *http.Request, p resources.Principal, in resources.DraftWrite) (any, error) {
		return s.UpdateDraft(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	write[resources.FileLink](h, m, "POST /api/v1/resources/items/{id}/file", func(r *http.Request, p resources.Principal, in resources.FileLink) (any, error) {
		return s.AttachFile(r.Context(), p, r.PathValue("id"), key(r), in)
	})
	write[resources.Command](h, m, "POST /api/v1/resources/items/{id}/submit", func(r *http.Request, p resources.Principal, in resources.Command) (any, error) {
		return s.Submit(r.Context(), p, r.PathValue("id"), key(r), in)
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
		n, e := limit(r)
		if e != nil {
			return nil, e
		}
		q := r.URL.Query()
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
	h.mount(m, "GET /api/v1/resources/items/{id}/download", h.download)
	h.mount(m, "GET /api/v1/resources/packages/{id}/parts/{part}", h.downloadPackage)
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
	m.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		p, e := h.auth.Authenticate(r)
		if e != nil || !p.Valid() {
			if errors.Is(e, resources.ErrUnavailable) {
				failure(w, e)
			} else {
				failure(w, resources.ErrUnauthenticated)
			}
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		r = r.WithContext(ctx)
		controller := http.NewResponseController(w)
		deadline, _ := ctx.Deadline()
		_ = controller.SetReadDeadline(deadline)
		_ = controller.SetWriteDeadline(deadline)
		stop := context.AfterFunc(ctx, func() { _ = controller.SetReadDeadline(time.Now()); _ = controller.SetWriteDeadline(time.Now()) })
		defer stop()
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
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
		// Authentication already succeeded above. This validates the contract's
		// required fields, nested shapes and limits against the same request body.
		if err = openapi3filter.ValidateRequest(ctx, &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route, Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, RejectWhenRequestBodyNotSpecified: true, SkipSettingDefaults: true}}); err != nil {
			failure(w, resources.ErrInput)
			return
		}
		out, e := run(w, r, p)
		if e != nil {
			failure(w, e)
			return
		}
		if out != nil {
			writeJSON(w, 200, out)
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
	out := resources.Filter{SchoolID: q.Get("school_id"), CourseID: q.Get("course_id"), CategoryID: q.Get("category_id"), DirectoryID: q.Get("directory_id"), Query: q.Get("query"), After: q.Get("after"), Limit: n}
	if e != nil {
		return out, e
	}
	if raw := q.Get("attributes"); raw != "" {
		if len(raw) > 16000 || json.Unmarshal([]byte(raw), &out.Attributes) != nil {
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
	}{{resources.ErrInput, 400}, {resources.ErrUnauthenticated, 401}, {resources.ErrDenied, 403}, {resources.ErrNotFound, 404}, {resources.ErrConflict, 409}, {resources.ErrCapacity, 413}} {
		if errors.Is(e, item.err) {
			status, code = item.status, item.err.Error()
			break
		}
	}
	writeJSON(w, status, map[string]string{"code": code})
}

type stream struct {
	w          http.ResponseWriter
	kind, name string
	written    bool
}

func (s *stream) Write(b []byte) (int, error) {
	if !s.written {
		s.w.Header().Set("Content-Type", s.kind)
		s.w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": s.name}))
		s.written = true
	}
	return s.w.Write(b)
}
func (h *handler) download(w http.ResponseWriter, r *http.Request, p resources.Principal) (any, error) {
	resource, e := h.s.Get(r.Context(), p, r.PathValue("id"))
	if e != nil {
		return nil, e
	}
	if !resource.DownloadAllowed || resource.Current == nil || resource.Current.File == nil {
		return nil, resources.ErrDenied
	}
	out := &stream{w: w, kind: "application/octet-stream", name: resource.Current.File.Filename}
	_, e = h.s.Download(r.Context(), p, resource.ID, out)
	if e != nil && out.written {
		panic(http.ErrAbortHandler)
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
