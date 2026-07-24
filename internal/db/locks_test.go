package db

import (
	"errors"
	"testing"

	"github.com/sauraku/devops-control/internal/models"
)

func TestReleaseLockRejectsMissingOrDifferentOwner(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := database.UpsertProject(&models.Project{
		ID: "medusa", Name: "Medusa", BranchName: "main",
	}); err != nil {
		t.Fatal(err)
	}
	lock := &models.DeployLock{
		ProjectID:   "medusa",
		OperationID: "deploy-owner",
		Operation:   "deploy",
	}
	if err := database.CreateLock(lock); err != nil {
		t.Fatal(err)
	}

	err = database.ReleaseLock(lock.ProjectID, "different-operation")
	var ownershipErr *LockOwnershipError
	if !errors.As(err, &ownershipErr) {
		t.Fatalf("wrong-owner release error = %v, want LockOwnershipError", err)
	}
	if ownershipErr.ProjectID != lock.ProjectID || ownershipErr.OperationID != "different-operation" {
		t.Fatalf("ownership error = %#v", ownershipErr)
	}
	if current, err := database.GetLock(lock.ProjectID); err != nil || current == nil || current.OperationID != lock.OperationID {
		t.Fatalf("wrong-owner release changed lock: lock=%#v err=%v", current, err)
	}

	if err := database.ReleaseLock(lock.ProjectID, lock.OperationID); err != nil {
		t.Fatal(err)
	}
	if err := database.ReleaseLock(lock.ProjectID, lock.OperationID); !errors.As(err, &ownershipErr) {
		t.Fatalf("duplicate release error = %v, want LockOwnershipError", err)
	}
}
