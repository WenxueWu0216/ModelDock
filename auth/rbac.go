// Package auth implements JWT authentication with role claims and the RBAC
// policy that maps roles to permissions.
package auth

import (
	"fmt"

	"github.com/wenxuewu0216/modeldock/domain"
)

// Permission is a capability checked by handlers.
type Permission string

const (
	PermRead     Permission = "read"     // list/get anything, audit log, dashboard
	PermRegister Permission = "register" // create models and versions
	PermDeploy   Permission = "deploy"   // deploy to staging, request promotion, promote approved, rollback
	PermApprove  Permission = "approve"  // approve/reject promotion requests
)

// Roles are deliberately not a strict hierarchy: an approver can approve but
// cannot deploy, and a deployer can deploy but cannot approve. This enforces
// separation of duties for production changes. Admin has everything, but the
// four-eyes rule (CanDecide) still stops an admin approving their own request.
var rolePermissions = map[domain.Role]map[Permission]bool{
	domain.RoleViewer:   {PermRead: true},
	domain.RoleDeployer: {PermRead: true, PermRegister: true, PermDeploy: true},
	domain.RoleApprover: {PermRead: true, PermApprove: true},
	domain.RoleAdmin:    {PermRead: true, PermRegister: true, PermDeploy: true, PermApprove: true},
}

// Can reports whether role has permission p.
func Can(role domain.Role, p Permission) bool {
	return rolePermissions[role][p]
}

// Require returns a domain.ErrForbidden-wrapped error if u lacks p.
func Require(u domain.User, p Permission) error {
	if !Can(u.Role, p) {
		return fmt.Errorf("%w: role %q lacks permission %q", domain.ErrForbidden, u.Role, p)
	}
	return nil
}

// CanDecide enforces the approval policy for a promotion request: the
// decider needs the approve permission, the request must still be pending,
// and nobody may decide on a promotion they requested themselves.
func CanDecide(u domain.User, a domain.Approval) error {
	if err := Require(u, PermApprove); err != nil {
		return err
	}
	if a.Status != domain.ApprovalPending {
		return fmt.Errorf("%w: approval %s is already %s", domain.ErrConflict, a.ID, a.Status)
	}
	if a.RequestedBy == u.ID {
		return fmt.Errorf("%w: %s requested this promotion and cannot approve it", domain.ErrForbidden, u.ID)
	}
	return nil
}
