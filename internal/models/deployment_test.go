package models

import "testing"

func TestDeploymentOperationPhases(t *testing.T) {
	tests := []struct {
		status                 DeploymentStatus
		phase                  DeploymentPhase
		terminal, successful   bool
		manualApprovalRequired bool
	}{
		{DeploymentStatusPending, DeploymentPhasePending, false, false, false},
		{DeploymentStatusPendingApproval, DeploymentPhaseManualApproval, false, false, true},
		{DeploymentStatusRunning, DeploymentPhaseRunning, false, false, false},
		{DeploymentStatusSuccess, DeploymentPhaseTerminal, true, true, false},
		{DeploymentStatusFailed, DeploymentPhaseTerminal, true, false, false},
		{DeploymentStatusAborted, DeploymentPhaseTerminal, true, false, false},
	}
	for _, test := range tests {
		t.Run(string(test.status), func(t *testing.T) {
			operation := NewDeploymentOperation(&Deployment{ID: "deploy-medusa-1", Status: test.status})
			if operation.Phase != test.phase || operation.Terminal != test.terminal ||
				operation.Successful != test.successful || operation.ManualApprovalRequired != test.manualApprovalRequired {
				t.Fatalf("operation = %#v", operation)
			}
		})
	}
}
