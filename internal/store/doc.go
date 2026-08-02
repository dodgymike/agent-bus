// Package store will own the durable state of the bus and its in-memory
// serving copy.
//
// Memory is the serving copy; disk is the truth (invariant 5). State is held
// in memory for speed and rebuilt by replaying the durable store on start.
// Writes are acknowledged only once committed through the two-phase
// (prepare -> commit) path and fsynced (invariant 4).
//
// Stub: the durability epic supplies the implementation.
package store
