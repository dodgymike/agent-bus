package ids

import (
	"fmt"
	"os"
)

// atomicWriteFile writes data to path via a temp file in the SAME directory:
// the temp file is created, chmodded 0600, written, fsynced and closed, renamed
// into place, and then the directory itself is fsynced so the rename is durable.
// A reader therefore sees either the complete old file or the complete new one,
// never a torn one. The temp file is removed on every error path.
//
// tmpPattern is passed to os.CreateTemp and should be a dotted, caller-specific
// pattern (".bus-id-*", ".agent-suffixes-*") so a temp file surviving a hard
// kill is identifiable rather than anonymous. It MUST resolve within dir — the
// temp file has to share a filesystem with path, or the rename stops being
// atomic and becomes a copy.
//
// # Why this is one function and not two
//
// This is the whole content of the unification task (Spec Server
// 1aed37a9-3a8e-4940-8b36-ee2dbe28afb5). Until it landed there were two
// byte-identical copies of this sequence in this package: writeBusIDFile in
// busid.go and atomicWriteFile in suffixstore.go, the second of which carried a
// comment saying the duplication was deliberate for that task's scope and that
// "if either copy changes, BOTH must".
//
// That is the failure mode being removed. Every step here is load-bearing and
// each one is a silent, undetectable-by-test omission if it is dropped from one
// copy and not the other:
//
//   - fsync of the FILE before the rename — without it the rename can be
//     durable while the bytes it points at are not, so a crash yields a
//     zero-length or short file that reads as a LOWER floor;
//   - fsync of the DIRECTORY after the rename — without it the rename itself
//     can be lost, so the file reverts to its previous content, which is again a
//     lower floor;
//   - the temp file living in dir rather than the system temp dir, so the rename
//     is an intra-filesystem link swap and not a copy;
//   - 0600 before any content is written, so the bytes are never briefly
//     world-readable.
//
// A test cannot see any of these missing on a machine that does not crash at the
// exact moment, so duplication here is not a style problem — it is a durability
// property that can rot in one copy while every test stays green. Both callers
// persist state that invariant 1 rests on (the bus's own id; the per-name agent
// id suffix floors, which are written AHEAD of the suffix they authorise), so
// there is exactly one correct sequence and it should exist exactly once.
//
// The caller supplies the bytes and their meaning; this function supplies only
// the atomic-replace-and-fsync. It does not create dir, and it does not
// interpret path.
func atomicWriteFile(dir, path, tmpPattern string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting mode on %s: %w", tmpName, err)
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}

	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpName, path, err)
	}

	dirFile, derr := os.Open(dir)
	if derr != nil {
		err = fmt.Errorf("opening %s to fsync directory entry: %w", dir, derr)
		return err
	}
	defer dirFile.Close()
	if serr := dirFile.Sync(); serr != nil {
		err = fmt.Errorf("syncing directory %s: %w", dir, serr)
		return err
	}
	return nil
}
