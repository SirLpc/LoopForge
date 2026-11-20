# WSL2 Warm Workspace Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans or superpowers:subagent-driven-development. Use test-driven development for behavior changes.

**Goal** Run the existing warm workspace pool on this WSL2 machine using an exclusive Docker Desktop Linux volume.

**Architecture** Keep the native path intact. Add explicit volume-aware task mounts and a Docker Desktop storage and process qualification policy. Wire each into the existing Runner-local pool and deploy only after isolated acceptance.

**Spec** docs/superpowers/specs/2026-11-20-wsl2-warm-workspace-design.md

### Task 1. Exact Docker workspace mounts

- [ ] Reproduce a missing subpath contract in runtime tests. Test resolved volume name and source, requested subpath, read/write mode and mount destination.
- [ ] Add driver support in a focused runtime file. Use it in sandbox creation, warm Git helpers and any separate warm browser mount. Reject unsupported access paths with errors. Verify exact mounts before a fresh start and on reuse. Preserve ordinary host bind behavior.
- [ ] Verify with disposable Docker Desktop volume and native bind regression tests.

### Task 2. Docker Desktop qualification and return proof

- [ ] Write failure tests for unknown overlapping mount, foreign volume or unverified Runner identity, and unavailable process inventory.
- [ ] Implement isolated Docker Desktop qualification in runtime. Verify local daemon and exclusive owned volume, verify mounted Runner identity, fence all overlapping containers including volume subpaths, and run an ephemeral scoped process inventory helper. Ordinary host checks remain on existing path.
- [ ] Verify real process inventory and retained-slot behavior using only disposable resources.

### Task 3. Wire Runner and deploy after acceptance

- [ ] Write Runner configuration/main and integration tests. Only local configuration can select the special mode, and unqualified storage must not advertise or reuse warm slots.
- [ ] Add CLI and locally provisioned runner tool paths. Connect volume requirements and release policy to existing runner lifecycle hooks. Document setup and rollback, using fictional public examples only.

### Task 4. Close review findings and verify conversation phases

- [ ] Test collision retention before close or recreation, and use verified immutable Docker identities for cleanup.
- [ ] Test that dedicated-mode controls deny unleased tasks and reject CLI directories outside the trusted tools directory.
- [ ] Verify the dedicated claim route leaves ordinary work queued for existing Runners and refuses to claim through older control planes.
- [ ] Verify named-volume sandbox creation, session-private mounts and approval guard files for both text and leased-repository phases. Exercise each phase with a stub provider CLI in the actual Docker Desktop. Keep native paths intact.
- [ ] Verify the isolated allocation, Docker execution, Git operations, release and recovery on the current Docker Desktop. Run full Go and scoped race regression. Request independent code review.
- [ ] Prepare two native-volume copies of the authorized Unity checkouts and configure the dedicated runner. Verify actual platform slot registration, readiness and project configuration through existing authenticated entry points. Preserve source paths and the current runner until the new route is validated.
