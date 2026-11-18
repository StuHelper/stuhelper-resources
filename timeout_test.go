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
