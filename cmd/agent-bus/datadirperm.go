package main

// The data-directory permission gate.
//
// # The defect it closes, and why it is a DIRECTORY problem
//
// Every identity file in the data directory is written 0600: bus-id,
// agent-suffixes, wal-mac.key, wal-index-floor, message-seq-floor. That mode
// answers "who may OPEN this file". It does not answer "who may REPLACE it" —
// unlinking a file and creating another in its place, or renaming over it, are
// permissions on the CONTAINING DIRECTORY, not on the file. Directory-write and
// file-read are independent bits.
//
// So a group- or other-writable data directory hands every local user in that
// set the power to substitute any identity file it holds, and the 0600 on the
// files does nothing about it. This is not a theoretical gap: it is exactly the
// privilege the forged message-seq-floor exploit needs (see
// seqfloorforge_test.go — a floor of 2^64-1 with a valid unkeyed digest boots a
// perfectly healthy-looking bus that 500s every send forever, across every
// restart).
//
// It also disposes of the argument that per-file digests need not be keyed
// "because an attacker with write access to the data directory can read the WAL
// MAC key sitting next to it anyway". They cannot: reading wal-mac.key needs
// FILE read on a 0600 file, replacing message-seq-floor needs DIRECTORY write.
// A user with the second and not the first is an ordinary local user on a
// 0777 data directory. Keying every file would still leave that user able to
// create, delete and rename — which is why this gate, and not more MACs, is the
// primary fix.
//
// # Why nothing enforced it before
//
// run() calls os.MkdirAll(cfg.DataDir, 0o700), and that looks like enough. It
// is not: MkdirAll returns nil and does NOTHING when the directory already
// exists — no chmod, no check, no warning. A pre-created 0777 data directory
// therefore stays 0777 through a completely clean start. The observed live data
// directory in this repo is 0775, a umask artefact nobody chose.
//
// The client has protected its own credential directory this way since MTLS
// (client/store.go stats, tightens and warns). The server did not protect its
// own. That asymmetry was the defect.
//
// # The two outcomes, and why they differ
//
// OTHER-writable  -> REFUSE to start.
// GROUP-writable  -> tighten to 0700 and WARN loudly.
//
// The reasoning is recorded in full in DECISIONS.md (2026-08-07); the short
// version is that the two differ on the SIZE of the trusted set and on the BASE
// RATE of a benign cause. "Group" grants a bounded set an administrator chose,
// and its overwhelmingly common cause is an accident a chmod fully removes;
// refusing would brick working buses on upgrade over a condition we can simply
// fix. "Other" grants an unbounded set — every account on the box, and anything
// that gets code execution as any of them — and has no benign cause whatsoever,
// since a directory this binary created can never be other-writable (see below).
//
// There is deliberately NO FLAG to bypass either branch. A flag that turns off
// a security check is a flag that ends up in somebody's unit file, and
// invariant 11 already states the posture: never ship the switch that silently
// disables verification.

import (
	"fmt"
	"io/fs"
	"os"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// dataDirMode is the only mode this server considers safe for its data
// directory: owner-only. It is what MkdirAll is asked for and what the tighten
// branch corrects to.
const dataDirMode fs.FileMode = 0o700

// enforceDataDirPermissions is the gate. It must be called AFTER the directory
// exists and BEFORE anything reads or writes inside it — before the lock file,
// before the bus id, before any key material — because everything after it
// depends on the directory not being substitutable underneath it.
//
// Calling it late would not merely weaken the check, it would change what a
// refusal means: a start that has already minted a bus id and written two
// private keys into a world-writable directory has leaked material that a
// refusal cannot take back. TestRunRefusesAnOtherWritableDataDir asserts the
// directory is still EMPTY after a refusal, which is what pins the ordering.
//
// It returns a fatal error for the refuse branch and nil for every other case,
// warning through lg on the tighten branch.
func enforceDataDirPermissions(dir string, lg *logging.Logger) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("checking the permissions of the data directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the data directory %s is not a directory", dir)
	}
	perm := info.Mode().Perm()

	// OTHER-writable: refuse. Checked first, so a 0777 directory takes this
	// branch rather than being quietly tightened by the group branch below.
	//
	// The sticky bit is deliberately NOT treated as mitigating. On a sticky
	// directory another user cannot unlink or rename OUR files — but it can
	// still CREATE files we have not written yet, and every identity file in
	// here is absent on a first start and after any loss. A message-seq-floor
	// planted before the bus ever writes one is the same exploit with no
	// unlink involved.
	if perm&0o002 != 0 {
		return fmt.Errorf("refusing to start: the data directory %s is mode %#o, which is WRITABLE BY ANY LOCAL USER. "+
			"Every identity file in it — the bus id, the agent-id suffix floors, the WAL MAC key, the WAL index floor and the message sequence floor — can be deleted and replaced by anyone on this machine, whatever mode the files themselves carry: replacing a file is a permission on the DIRECTORY, not on the file. "+
			"A forged message sequence floor alone is enough to make this bus start perfectly healthy and then fail every send, permanently. "+
			"This is a refusal rather than an automatic correction because a directory that has been world-writable may ALREADY have had a file substituted, and adopting a forged one is silent and undetectable — the mode is left untouched so you can see it. "+
			"Remedy: run: chmod 700 %s — then, if this directory was exposed on a machine with untrusted local users, treat its key material as compromised (re-issue invites carrying the bus certificate fingerprint) before restarting",
			dir, perm, dir)
	}

	// GROUP-writable: correct it, and make sure the operator hears about it.
	if perm&0o020 != 0 {
		if err := os.Chmod(dir, dataDirMode); err != nil {
			return fmt.Errorf("refusing to start: the data directory %s is mode %#o, which is writable by its group, and it could not be tightened to %#o: %w. "+
				"Any local user in that group can delete and replace the identity files in it, whatever mode the files themselves carry. "+
				"Remedy: run: chmod 700 %s (as its owner) and restart",
				dir, perm, dataDirMode, err, dir)
		}
		// WARN, not INFO. Once the chmod above has run, this line is the ONLY
		// surviving evidence that the directory was ever writable by anyone but
		// its owner — the mode that would have shown it has just been corrected
		// out of existence. Emitting it quietly would reproduce the silent
		// discard invariant 6 rates a P0.
		lg.Warn("data directory permissions tightened: it was writable by its GROUP, so any local user in that group could delete and replace the identity files in it (the bus id, the agent-id suffix floors, the WAL MAC key, the WAL index floor and the message sequence floor) regardless of those files' own 0600 mode, because replacing a file is a permission on the DIRECTORY. The mode has been corrected for you and the start continues, because the overwhelmingly common cause is an accident (a umask-002 mkdir, or a deployment group) that this chmod fully removes. If untrusted users were ever in that group, treat this directory's key material as compromised: check the bus certificate fingerprint against what your clients have pinned, and re-issue invites if it has moved",
			"data_dir", dir,
			"was_mode", fmt.Sprintf("%#o", perm),
			"now_mode", fmt.Sprintf("%#o", dataDirMode),
		)
		return nil
	}

	// Anything else — 0700, 0750, 0755 — is left EXACTLY as the operator set
	// it. The subject of this gate is WRITE. Group- or other-READ on the
	// directory discloses only the file NAMES, which are fixed and documented
	// in CONTRACTS-ONDISK.md anyway; the contents stay behind each file's own
	// 0600. Tightening those would be a change to configuration the operator
	// deliberately chose, for no security gain.
	return nil
}
