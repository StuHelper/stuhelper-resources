package resources_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"github.com/StuHelper/stuhelper-resources/sdk"
)

type instructorAliasCatalog struct {
	*catalog
	mu      sync.Mutex
	allowed map[string]bool
}

func (c *instructorAliasCatalog) Validate(ctx context.Context, a r.Associations) error {
	if a.SchoolID == "school" && (a.CourseID == "course" || a.CourseID == "legacy-course") {
		return nil
	}
	return c.catalog.Validate(ctx, a)
}

func (c *instructorAliasCatalog) CourseReferences(_ context.Context, _ r.Principal, school string, ids []string) (map[string][]string, error) {
	if school != "school" {
		return nil, r.ErrDenied
	}
	out := map[string][]string{}
	for _, id := range ids {
		switch id {
		case "course", "legacy-course":
			out[id] = []string{"course", "legacy-course"}
		default:
			return nil, r.ErrNotFound
		}
	}
	return out, nil
}

func (c *instructorAliasCatalog) VerifyCourseInstructor(_ context.Context, p r.Principal, a r.Associations) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.allowed[p.Key()+"|"+a.SchoolID+"|"+a.CourseID] {
		return nil
	}
	return r.ErrDenied
}

func TestCourseDuplicateDigestCheckAndVerifiedInstructorAttribution(t *testing.T) {
	h := newFixture(t)

	// Without InstructorCatalog wired, VerifiedInstructor=true fails closed with ErrUnavailable.
	metaMissingPort := metadata("public")
	metaMissingPort.Origin.VerifiedInstructor = true
	if _, err := h.s.CreateDraft(h.ctx, uploader, "no-instructor-port", r.DraftWrite{Metadata: metaMissingPort}); !errors.Is(err, r.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable when InstructorCatalog absent, got %v", err)
	}

	ic := &instructorAliasCatalog{
		catalog: h.catalog,
		allowed: map[string]bool{
			uploader.Key() + "|school|course":        true,
			uploader.Key() + "|school|legacy-course": true,
		},
	}
	var err error
	h.s, err = r.New(h.repo, r.Dependencies{Authority: h.auth, Catalog: ic, Files: h.files, Moderation: h.moderation}, h.cfg)
	must(t, err)

	handler, err := httpapi.New(h.s, authenticator{})
	must(t, err)
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := sdk.NewClientWithResponses(server.URL)
	must(t, err)

	targetDigest := strings.Repeat("d", 64)
	seedFile := func(id string, owner r.Principal, digest string) {
		file := r.File{VersionID: id, Filename: id + ".pdf", ContentType: "application/pdf", Size: 60, SHA256: digest}
		h.files.mu.Lock()
		h.files.items[id] = r.FileEvidence{File: file, Owner: owner, Module: "resources", SchoolID: "school", State: "awaiting_approval", Access: "public"}
		h.files.mu.Unlock()
	}

	// VerifiedInstructor requires CourseID; non-instructor student is rejected.
	metaNoCourse := metadata("public")
	metaNoCourse.Associations.CourseID = ""
	metaNoCourse.Origin.VerifiedInstructor = true
	if _, err := h.s.CreateDraft(h.ctx, uploader, "instructor-no-course", r.DraftWrite{Metadata: metaNoCourse}); !errors.Is(err, r.ErrInput) {
		t.Fatalf("expected ErrInput without course_id, got %v", err)
	}
	metaStudent := metadata("public")
	metaStudent.Origin.VerifiedInstructor = true
	if _, err := h.s.CreateDraft(h.ctx, reader, "student-impersonate", r.DraftWrite{Metadata: metaStudent}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected ErrDenied for unverified student, got %v", err)
	}

	// Publish a verified instructor resource on legacy-course alias with targetDigest.
	metaInstructor := metadata("public")
	metaInstructor.Associations.CourseID = "legacy-course"
	metaInstructor.Origin = r.Origin{Kind: "known", Source: "任课教师教学大纲", VerifiedInstructor: true}
	instDraft, err := h.s.CreateDraft(h.ctx, uploader, "inst-draft", r.DraftWrite{Metadata: metaInstructor})
	must(t, err)
	if instDraft.Proposal.VerifiedInstructor {
		t.Fatal("draft before submit should not carry verified snapshot yet")
	}
	seedFile("file-inst-1", uploader, targetDigest)
	instDraft, err = h.s.AttachFile(h.ctx, uploader, instDraft.ID, "attach-inst-1", r.FileLink{Revision: instDraft.Revision, MediaVersionID: "file-inst-1"})
	must(t, err)
	instPub := h.publish(t, instDraft)
	if instPub.Current == nil || !instPub.Current.VerifiedInstructor {
		t.Fatalf("published instructor resource missing VerifiedInstructor snapshot: %+v", instPub.Current)
	}

	// Also create an unpublished draft with the same SHA256 -> must NOT appear in duplicate results.
	unpublished, err := h.s.CreateDraft(h.ctx, uploader, "unpub-dup", r.DraftWrite{Metadata: metadata("public")})
	must(t, err)
	seedFile("file-unpub-dup", uploader, targetDigest)
	_, err = h.s.AttachFile(h.ctx, uploader, unpublished.ID, "attach-unpub-dup", r.FileLink{Revision: unpublished.Revision, MediaVersionID: "file-unpub-dup"})
	must(t, err)

	// Query duplicates on canonical course via generated SDK (matches legacy-course alias!).
	dupOwnerResp, err := client.FindDuplicateResourcesWithResponse(h.ctx, &sdk.FindDuplicateResourcesParams{
		SchoolId: "school",
		CourseId: "course",
		Sha256:   targetDigest,
	}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer owner")
		return nil
	})
	must(t, err)
	if dupOwnerResp.StatusCode() != 200 || dupOwnerResp.JSON200 == nil || len(dupOwnerResp.JSON200.Items) != 1 || dupOwnerResp.JSON200.Items[0].Id != instPub.ID || !dupOwnerResp.JSON200.Items[0].Owned {
		t.Fatalf("owner duplicate check mismatch: status=%d body=%s", dupOwnerResp.StatusCode(), dupOwnerResp.Body)
	}

	dupReaderResp, err := client.FindDuplicateResourcesWithResponse(h.ctx, &sdk.FindDuplicateResourcesParams{
		SchoolId: "school",
		CourseId: "course",
		Sha256:   targetDigest,
	}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer reader")
		return nil
	})
	must(t, err)
	if dupReaderResp.StatusCode() != 200 || dupReaderResp.JSON200 == nil || len(dupReaderResp.JSON200.Items) != 1 || dupReaderResp.JSON200.Items[0].Owned {
		t.Fatalf("reader duplicate check mismatch: status=%d body=%s", dupReaderResp.StatusCode(), dupReaderResp.Body)
	}

	// Duplicate check requires both upload and discover permissions, and unlisting hides duplicates.
	h.auth.set("upload", true)
	if _, err := h.s.FindDuplicates(h.ctx, reader, "school", "course", targetDigest); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("expected upload permission denial on duplicate check, got %v", err)
	}
	h.auth.set("upload", false)

	// Revoking instructor binding before Submit rejects submission.
	revokedDraft, err := h.s.CreateDraft(h.ctx, uploader, "inst-revoked-before-submit", r.DraftWrite{Metadata: metaInstructor})
	must(t, err)
	seedFile("file-inst-2", uploader, strings.Repeat("e", 64))
	revokedDraft, err = h.s.AttachFile(h.ctx, uploader, revokedDraft.ID, "attach-inst-2", r.FileLink{Revision: revokedDraft.Revision, MediaVersionID: "file-inst-2"})
	must(t, err)
	ic.mu.Lock()
	ic.allowed[uploader.Key()+"|school|legacy-course"] = false
	ic.mu.Unlock()
	if _, err := h.s.Submit(h.ctx, uploader, revokedDraft.ID, "submit-after-revoke", r.Command{Revision: revokedDraft.Revision}); !errors.Is(err, r.ErrDenied) {
		t.Fatalf("revoked instructor should be denied on Submit, got %v", err)
	}

	// Closing uploader account strips VerifiedInstructor disclosure while preserving published content.
	edit := r.StateChange{Revision: instPub.Revision, State: "published", Reason: "确认发布"}
	original, err := h.s.SetState(h.ctx, manager, instPub.ID, "confirm-instructor", edit)
	must(t, err)
	if original.Current == nil || !original.Current.VerifiedInstructor {
		t.Fatal("initial response lacks instructor snapshot")
	}
	must(t, h.s.AnonymizeOwner(h.ctx, uploader, "33333333-3333-4333-8333-333333333333"))
	replayed, err := h.s.SetState(h.ctx, manager, instPub.ID, "confirm-instructor", edit)
	must(t, err)
	if replayed.Revision != original.Revision || replayed.Current == nil || replayed.Current.VerifiedInstructor || replayed.Current.Metadata.Origin.VerifiedInstructor {
		t.Fatal("replay exposed instructor after closure")
	}
	afterClosure, err := h.s.Get(h.ctx, reader, instPub.ID)
	must(t, err)
	if afterClosure.Current == nil || afterClosure.Current.VerifiedInstructor || afterClosure.Current.Metadata.Origin.VerifiedInstructor {
		t.Fatalf("closed owner retained VerifiedInstructor disclosure: %+v", afterClosure.Current)
	}
}
