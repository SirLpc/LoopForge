# Native fixed-directory warm workspaces

The Runner leases the original checkout and exposes it in its private mount namespace at the standard per-run repository path. Windows Unity continues to see the original project and Library. The Runner and Git cache remain on the Linux filesystem. The native adapter is opt-in and scoped to dedicated personal Runner use.

- [x] Test file identity and persistence through placement, restart and return. Implement verified bind mounts and recovery in a Runner-local adapter.
- [x] Test Git preparation against a fixed checkout across filesystems and protect dirty state. Reuse the existing verified checkout and return proofs. Keep Git staging separate from source and Library files.
- [x] Route the native profile through the existing lease protocol, bwrap sandbox and Git helper. Check that legacy Docker pool behavior still passes.
- [x] Exercise the Runner in an isolated WSL2 mount namespace with a disposable checkout and document startup and operator handoff.
