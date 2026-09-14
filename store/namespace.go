package store

// Repository lifecycle roles and per-namespace ownership live in rbac.go and
// user.go. The legacy namespace_members ACL and branch ACL models were removed:
// collaborative access is now expressed by repo_members (maintainer /
// developer) plus the namespace owner (owner).
