package resources_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	r "github.com/StuHelper/stuhelper-resources"
	"github.com/StuHelper/stuhelper-resources/httpapi"
	"github.com/StuHelper/stuhelper-resources/sdk"
)

func TestManagedResourceStateFailedJobFiltersCoverageListAndCourseDirectoryTree(t *testing.T) {
	h := newFixture(t)
	handler, err := httpapi.New(h.s, authenticator{})
	must(t, err)
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := sdk.NewClientWithResponses(server.URL)
	must(t, err)

	// 1. Seed published, unlisted, and exhausted-publish-job resources.
	pubItem := h.publish(t, h.draft(t, "adm-pub", "public"))
	unlistItem := h.publish(t, h.draft(t, "adm-unlist", "public"))
	unlistItem, err = h.s.SetState(h.ctx, manager, unlistItem.ID, "adm-unlist-op", r.StateChange{Revision: unlistItem.Revision, State: "unlisted", Reason: "整理下架"})
	must(t, err)

	failedItem := h.draft(t, "adm-failed", "public")
	failedItem, err = h.s.Submit(h.ctx, uploader, failedItem.ID, "adm-submit", r.Command{Revision: failedItem.Revision})
	must(t, err)
	h.drain(t)
	failedItem, err = h.s.ApplyDecision(h.ctx, manager, failedItem.ID, "adm-approve", r.Decision{ID: "adm-dec", Revision: failedItem.Revision, VersionID: failedItem.Proposal.ID, Action: "approve"})
	must(t, err)
	h.files.fail = true
	for i := 0; i < 3; i++ {
		h.clock.Add(time.Minute)
		ok, err := h.s.ProcessOne(h.ctx, "worker")
		if !ok || !errors.Is(err, r.ErrUnavailable) {
			t.Fatalf("expected worker attempt failure, got ok=%v err=%v", ok, err)
		}
	}
	h.files.fail = false

	// Non-admin list endpoints reject admin-only filter parameters.
	if _, err := h.s.List(h.ctx, reader, r.Filter{SchoolID: "school", State: "unlisted", Limit: 10}, false, false); !errors.Is(err, r.ErrInput) {
		t.Fatalf("public list accepted state filter: %v", err)
	}
	if _, err := h.s.List(h.ctx, uploader, r.Filter{FailedOnly: true, Limit: 10}, true, false); !errors.Is(err, r.ErrInput) {
		t.Fatalf("own list accepted failed_only filter: %v", err)
	}

	// Owner (non-manager) does not receive internal operation_state / operation_error_code.
	ownerView, err := h.s.Get(h.ctx, uploader, failedItem.ID)
	must(t, err)
	if ownerView.OperationState != "" || ownerView.OperationErrorCode != "" {
		t.Fatalf("non-admin owner saw operation fields: %+v", ownerView)
	}

	// Admin filters by state, proposal_state, and failed_only via generated SDK.
	schoolID := "school"
	unlistedState := sdk.ListManagedResourcesParamsState("unlisted")
	unlistedResp, err := client.ListManagedResourcesWithResponse(h.ctx, &sdk.ListManagedResourcesParams{
		SchoolId: &schoolID,
		State:    &unlistedState,
	}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer global")
		return nil
	})
	must(t, err)
	if unlistedResp.StatusCode() != 200 || unlistedResp.JSON200 == nil || unlistedResp.JSON200.Total != 1 || unlistedResp.JSON200.Items[0].Id != unlistItem.ID {
		t.Fatalf("admin state=unlisted filter mismatch: status=%d body=%s", unlistedResp.StatusCode(), unlistedResp.Body)
	}

	failedOnly := true
	preparingState := sdk.ListManagedResourcesParamsProposalState("preparing")
	failedResp, err := client.ListManagedResourcesWithResponse(h.ctx, &sdk.ListManagedResourcesParams{
		SchoolId:      &schoolID,
		ProposalState: &preparingState,
		FailedOnly:    &failedOnly,
	}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer global")
		return nil
	})
	must(t, err)
	if failedResp.StatusCode() != 200 || failedResp.JSON200 == nil || failedResp.JSON200.Total != 1 || len(failedResp.JSON200.Items) != 1 {
		t.Fatalf("admin failed_only filter mismatch: status=%d body=%s", failedResp.StatusCode(), failedResp.Body)
	}
	gotFailed := failedResp.JSON200.Items[0]
	if gotFailed.Id != failedItem.ID || gotFailed.OperationState == nil || *gotFailed.OperationState != sdk.ResourceOperationState("failed") || gotFailed.OperationErrorCode == nil || *gotFailed.OperationErrorCode != sdk.ResourceOperationErrorCode("resources_dependency_failed") {
		t.Fatalf("expected failed operation fields on admin item, got %+v", gotFailed)
	}
	_ = pubItem

	retried, err := h.s.RetryPending(h.ctx, manager, failedItem.ID, "adm-retry", r.Command{Revision: failedItem.Revision})
	must(t, err)
	if retried.OperationState != "pending" || retried.OperationErrorCode != "" {
		t.Fatalf("expected pending operation_state after retry, got %+v", retried)
	}

	t.Run("old review failure", func(t *testing.T) {
		result, err := h.admin.Exec(`UPDATE resources.jobs SET state='failed',data=jsonb_set(data,'{State}','"failed"') WHERE data->>'ResourceID'=$1 AND data->>'Kind'='review'`, failedItem.ID)
		must(t, err)
		n, err := result.RowsAffected()
		must(t, err)
		if n != 1 {
			t.Fatalf("updated %d review jobs", n)
		}
		page, err := h.s.List(h.ctx, manager, r.Filter{SchoolID: "school", FailedOnly: true, Limit: 10}, false, true)
		must(t, err)
		if page.Total != 0 || len(page.Items) != 0 {
			t.Fatalf("stale review job matched: total=%d", page.Total)
		}
	})

	// 2. Coverage paginated list (requires coverage.manage).
	for _, plan := range []string{"plan-b", "plan-a"} {
		_, err := h.s.PutCoverage(h.ctx, global, plan, "seed-"+plan, r.Coverage{PlanID: plan, Rules: []r.CoverageRule{{SchoolID: "school"}}})
		must(t, err)
	}
	covDenied, err := client.ListCoverageWithResponse(h.ctx, nil, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer reader")
		return nil
	})
	must(t, err)
	if covDenied.StatusCode() != 403 {
		t.Fatalf("expected 403 on coverage list for reader, got %d", covDenied.StatusCode())
	}
	limitOne := "1"
	covPage1, err := client.ListCoverageWithResponse(h.ctx, &sdk.ListCoverageParams{Limit: &limitOne}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer global")
		return nil
	})
	must(t, err)
	if covPage1.StatusCode() != 200 || covPage1.JSON200 == nil || len(covPage1.JSON200.Items) != 1 || covPage1.JSON200.Items[0].PlanId != "plan-a" || covPage1.JSON200.NextCursor != "plan-a" {
		t.Fatalf("coverage page 1 mismatch: status=%d body=%s", covPage1.StatusCode(), covPage1.Body)
	}
	afterCov := covPage1.JSON200.NextCursor
	covPage2, err := client.ListCoverageWithResponse(h.ctx, &sdk.ListCoverageParams{Limit: &limitOne, After: &afterCov}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer global")
		return nil
	})
	must(t, err)
	if covPage2.StatusCode() != 200 || covPage2.JSON200 == nil || len(covPage2.JSON200.Items) != 1 || covPage2.JSON200.Items[0].PlanId != "plan-b" || covPage2.JSON200.NextCursor != "" {
		t.Fatalf("coverage page 2 mismatch: status=%d body=%s", covPage2.StatusCode(), covPage2.Body)
	}

	// 3. Course directory full tree query (tree=true).
	_, err = h.s.PutDirectory(h.ctx, manager, "d-root", "dir-root", r.DirectoryWrite{SchoolID: "school", CourseID: "course", Names: r.Names{"zh-CN": "课件"}, Enabled: true})
	must(t, err)
	_, err = h.s.PutDirectory(h.ctx, manager, "d-child", "dir-child", r.DirectoryWrite{SchoolID: "school", CourseID: "course", ParentID: "d-root", Names: r.Names{"zh-CN": "第一章"}, Enabled: true})
	must(t, err)
	_, err = h.s.PutDirectory(h.ctx, manager, "d-disabled", "dir-dis", r.DirectoryWrite{SchoolID: "school", CourseID: "course", Names: r.Names{"zh-CN": "停用目录"}, Enabled: false})
	must(t, err)

	treeTrue := true
	badTreeResp, err := client.ListDirectoriesWithResponse(h.ctx, &sdk.ListDirectoriesParams{SchoolId: &schoolID, Tree: &treeTrue}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer reader")
		return nil
	})
	must(t, err)
	if badTreeResp.StatusCode() != 400 {
		t.Fatalf("expected 400 for tree=true without course_id, got %d", badTreeResp.StatusCode())
	}

	courseID := "course"
	readerTree, err := client.ListDirectoriesWithResponse(h.ctx, &sdk.ListDirectoriesParams{SchoolId: &schoolID, CourseId: &courseID, Tree: &treeTrue}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer reader")
		return nil
	})
	must(t, err)
	if readerTree.StatusCode() != 200 || readerTree.JSON200 == nil || len(readerTree.JSON200.Items) != 2 || readerTree.JSON200.Items[0].Id != "d-root" || readerTree.JSON200.Items[1].Id != "d-child" {
		t.Fatalf("reader directory tree mismatch: status=%d body=%s", readerTree.StatusCode(), readerTree.Body)
	}

	adminTree, err := client.ListDirectoriesWithResponse(h.ctx, &sdk.ListDirectoriesParams{SchoolId: &schoolID, CourseId: &courseID, Tree: &treeTrue}, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer global")
		return nil
	})
	must(t, err)
	if adminTree.StatusCode() != 200 || adminTree.JSON200 == nil || len(adminTree.JSON200.Items) != 3 {
		t.Fatalf("admin directory tree should include disabled nodes: status=%d body=%s", adminTree.StatusCode(), adminTree.Body)
	}
	t.Run("school parent visibility", func(t *testing.T) {
		parent := r.DirectoryWrite{SchoolID: "school", Names: r.Names{"zh-CN": "共享目录"}, Enabled: true}
		shared, err := h.s.PutDirectory(h.ctx, manager, "shared-parent", "make-shared", parent)
		must(t, err)
		child := r.DirectoryWrite{SchoolID: "school", CourseID: "course", ParentID: shared.ID, Names: r.Names{"zh-CN": "作业"}, Enabled: true}
		_, err = h.s.PutDirectory(h.ctx, manager, "course-under-shared", "make-child", child)
		must(t, err)
		for _, enabled := range []bool{true, false} {
			if !enabled {
				parent.Revision = shared.Revision
				parent.Enabled = false
				_, err = h.s.PutDirectory(h.ctx, manager, shared.ID, "disable-shared", parent)
				must(t, err)
			}
			tree, err := h.s.CourseDirectoryTree(h.ctx, reader, "school", "course")
			must(t, err)
			has := false
			for _, d := range tree.Items {
				if d.ID == "course-under-shared" {
					has = true
				}
				if d.ID == shared.ID {
					t.Fatal("school parent in course-only result")
				}
			}
			if has != enabled {
				t.Fatalf("visible course child=%t parent enabled=%t", has, enabled)
			}
		}
	})

}
