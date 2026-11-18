package resources_test

import (
	"bytes"
	"context"
	"errors"
	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type pacedFiles struct {
	*files
	chunks   int
	interval time.Duration
	stall    bool
	inspectE error
}

func (f *pacedFiles) stream(ctx context.Context, payload string, w io.Writer) error {
	if f.stall {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
			return nil
		}
	}
	for i := 0; i < f.chunks; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.interval):
		}
		if _, err := io.WriteString(w, payload); err != nil {
			return err
		}
	}
	return nil
}

func (f *pacedFiles) Download(ctx context.Context, _ r.Principal, id string, w io.Writer) error {
	return f.stream(ctx, id+":", w)
}

func (f *pacedFiles) Archive(ctx context.Context, _ r.Principal, items []r.Selection, w io.Writer) error {
	return f.stream(ctx, items[0].ResourceID+":", w)
}

func (f *pacedFiles) ReviewDownload(ctx context.Context, _ r.Principal, _, id string, w io.Writer) error {
	return f.stream(ctx, id+":", w)
}

func (f *pacedFiles) Inspect(ctx context.Context, p r.Principal, id string) (r.FileEvidence, error) {
	if f.inspectE != nil {
		return r.FileEvidence{}, f.inspectE
	}
	return f.files.Inspect(ctx, p, id)
}

type slowModeration struct {
	*moderation
	hangStatus bool
}

func (m *slowModeration) Status(ctx context.Context, p r.Principal, id string) (r.RequestStatus, error) {
	if m.hangStatus {
		select {
		case <-ctx.Done():
			return r.RequestStatus{}, ctx.Err()
		case <-time.After(3 * time.Second):
			return r.RequestStatus{}, errors.New("unexpected raw moderation error")
		}
	}
	return m.moderation.Status(ctx, p, id)
}

type slowCatalog struct {
	*catalog
	hangRefs bool
}

func (c *slowCatalog) CourseReferences(ctx context.Context, _ r.Principal, _ string, _ []string) (map[string][]string, error) {
	if c.hangRefs {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
			return nil, errors.New("unexpected raw catalog error")
		}
	}
	return map[string][]string{"course": {"course"}}, nil
}

func TestStreamProgressAndMetadataRPCTimeouts(t *testing.T) {
	h := newFixture(t)
	pf := &pacedFiles{files: h.files, chunks: 6, interval: 250 * time.Millisecond}
	sm := &slowModeration{moderation: h.moderation}
	sc := &slowCatalog{catalog: h.catalog}
	var e error
	h.s, e = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: sc, Files: pf, Moderation: sm}, h.cfg)
	must(t, e)

	item := h.publish(t, h.draft(t, "stream-ok", "public"))
	pkg, e := h.s.CreatePackage(h.ctx, reader, "pkg-stream", r.PackageRequest{ResourceIDs: []string{item.ID}})
	must(t, e)

	// 6 * 250ms = 1.5s > 1s OperationTimeout: active streaming must succeed without truncation.
	var dlBuf bytes.Buffer
	_, e = h.s.Download(h.ctx, reader, item.ID, &dlBuf)
	must(t, e)
	if dlBuf.Len() == 0 {
		t.Fatal("empty download body")
	}

	var pkgBuf bytes.Buffer
	must(t, h.s.DownloadPackage(h.ctx, reader, pkg.ID, 1, &pkgBuf))
	if pkgBuf.Len() == 0 {
		t.Fatal("empty package body")
	}

	var revBuf bytes.Buffer
	must(t, h.s.DownloadReviewFile(h.ctx, manager, item.Current.ID, &revBuf))
	if revBuf.Len() == 0 {
		t.Fatal("empty review download body")
	}

	// HTTP streaming download also succeeds past OperationTimeout.
	handler, e := httpapi.New(h.s, authenticator{})
	must(t, e)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	req, e := http.NewRequest("GET", srv.URL+"/api/v1/resources/items/"+item.ID+"/download", nil)
	must(t, e)
	req.Header.Set("Authorization", "Bearer reader")
	resp, e := srv.Client().Do(req)
	must(t, e)
	body, e := io.ReadAll(resp.Body)
	resp.Body.Close()
	must(t, e)
	if resp.StatusCode != 200 || len(body) != dlBuf.Len() {
		t.Fatalf("HTTP stream download mismatch: status=%d len=%d want=%d", resp.StatusCode, len(body), dlBuf.Len())
	}

	// Stalled stream (> 1s without any write) must be aborted promptly with ErrUnavailable.
	pf.stall = true
	start := time.Now()
	if _, e = h.s.Download(h.ctx, reader, item.ID, &bytes.Buffer{}); !errors.Is(e, r.ErrUnavailable) || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("stalled Download expected ErrUnavailable within OperationTimeout, got err=%v elapsed=%v", e, time.Since(start))
	}
	start = time.Now()
	if e = h.s.DownloadPackage(h.ctx, reader, pkg.ID, 1, &bytes.Buffer{}); !errors.Is(e, r.ErrUnavailable) || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("stalled DownloadPackage expected ErrUnavailable within OperationTimeout, got err=%v elapsed=%v", e, time.Since(start))
	}
	start = time.Now()
	if e = h.s.DownloadReviewFile(h.ctx, manager, item.Current.ID, &bytes.Buffer{}); !errors.Is(e, r.ErrUnavailable) || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("stalled DownloadReviewFile expected ErrUnavailable within OperationTimeout, got err=%v elapsed=%v", e, time.Since(start))
	}
	pf.stall = false

	// Synchronous metadata RPCs (Moderation.Status and CourseReferences) must apply OperationTimeout and normalize to ErrUnavailable.
	receipt, e := h.s.CreateRequest(h.ctx, uploader, item.ID, "req-timeout", r.Request{Kind: "withdrawal", Reason: "更新版本"})
	must(t, e)
	h.drain(t)

	sm.hangStatus = true
	start = time.Now()
	if _, e = h.s.RequestStatus(h.ctx, uploader, receipt.ID); !errors.Is(e, r.ErrUnavailable) || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("RequestStatus expected ErrUnavailable within OperationTimeout, got err=%v elapsed=%v", e, time.Since(start))
	}

	sc.hangRefs = true
	start = time.Now()
	if _, e = h.s.Counts(h.ctx, reader, r.Filter{SchoolID: "school"}, []string{"course"}); !errors.Is(e, r.ErrUnavailable) || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("Counts CourseReferences expected ErrUnavailable within OperationTimeout, got err=%v elapsed=%v", e, time.Since(start))
	}

	// Raw non-domain dependency error on Files.Inspect normalizes to ErrUnavailable.
	d, e := h.s.CreateDraft(h.ctx, uploader, "inspect-err", r.DraftWrite{Metadata: metadata("public")})
	must(t, e)
	pf.inspectE = errors.New("raw downstream transport failure")
	if _, e = h.s.AttachFile(h.ctx, uploader, d.ID, "attach-err", r.FileLink{Revision: d.Revision, MediaVersionID: "m1"}); !errors.Is(e, r.ErrUnavailable) {
		t.Fatalf("AttachFile expected ErrUnavailable on raw error, got %v", e)
	}
}

type stalledPreflight struct {
	r.Repository
	cancelled bool
}

func (s *stalledPreflight) Transaction(ctx context.Context, _ func(r.Transaction) error) error {
	select {
	case <-ctx.Done():
		s.cancelled = true
	case <-time.After(1500 * time.Millisecond):
	}
	return r.ErrUnavailable
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
	atWrite  time.Duration
}

func (w *deadlineRecorder) SetWriteDeadline(d time.Time) error {
	w.deadline = d
	return nil
}
func (w *deadlineRecorder) Write(b []byte) (int, error) {
	w.atWrite = time.Until(w.deadline)
	return w.ResponseRecorder.Write(b)
}

func TestDownloadPreflightAndHTTPWriteAreBounded(t *testing.T) {
	h := newFixture(t)
	item := h.publish(t, h.draft(t, "http-bounds", "public"))
	deps := r.Dependencies{Authority: h.auth, Catalog: h.catalog, Files: h.files, Moderation: h.moderation}
	cfg := h.cfg
	cfg.OperationTimeout = time.Second
	req := httptest.NewRequest("GET", "/api/v1/resources/items/"+item.ID+"/download", nil)
	req.Header.Set("Authorization", "Bearer reader")
	t.Run("preflight", func(t *testing.T) {
		db := &stalledPreflight{Repository: h.repo}
		svc, err := r.New(db, deps, cfg)
		must(t, err)
		handler, err := httpapi.New(svc, authenticator{})
		must(t, err)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if !db.cancelled || rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("cancelled=%t status=%d", db.cancelled, rec.Code)
		}
	})
	t.Run("write deadline", func(t *testing.T) {
		svc, err := r.New(h.repo, deps, cfg)
		must(t, err)
		handler, err := httpapi.New(svc, authenticator{})
		must(t, err)
		rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 || rec.atWrite <= 0 || rec.atWrite > cfg.OperationTimeout {
			t.Fatalf("status=%d bytes=%d write deadline=%v", rec.Code, rec.Body.Len(), rec.atWrite)
		}
	})
}
