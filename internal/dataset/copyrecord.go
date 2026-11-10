// Package dataset implements the S3 release mechanism: a dataset is copied out of the
// Ceph source into a hot cache bucket of its own, and researcher accounts are granted read
// access to that bucket from their nominated VPCs through the bucket policy and the policy
// of the KMS key encrypting it.
//
// The ledger holds two kinds of record, and the split is the shape of the whole package. A
// copy says what was transferred and where it staged; a grant says who may read what a copy
// delivered, from where, and until when. Several grants stand against one copy, and the
// copy outlives all of them.
//
// Access is enforced by S3 and KMS rather than by anything in this process, so a record
// here declares the access that is intended; the deployed policies are what a request is
// actually checked against. The two can drift, which is why reconciliation exists.
//
// Neither record shares a type with the htsget share record. The mechanisms keep one ledger
// table so that "what can account X read?" has a single answer, but they hold different
// fields, enforce expiry by different means, and have different lifecycles. They meet only
// in the columns a listing prints.
package dataset

// The ledger record for one copy of one dataset into this account.
//
// A copy and a grant are separate records because they are separate things with separate
// lives. Copying a dataset is a transfer that runs for hours and is billed for it; granting
// access to what it delivered is a policy statement written in a second. One copy is
// granted, withdrawn, and granted again -- to the same researcher or another -- without
// anything being transferred a second time, so a record that meant both would either have
// to be rewritten for each grant, losing the history of the ones before it, or force a
// fresh copy to record a fresh grant.
//
// What a copy record owns is the provenance of the bytes: which dataset, which bucket and
// key they staged in, which execution moved them, how many objects and how much data
// arrived, and where the copy's own summary of that is kept. None of it changes once the
// copy has reported. What it does not own is who may read them.

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// CopyState is the lifecycle position of one copy.
type CopyState string

const (
	// CopyStateCopying means the transfer has been started and has not reported a
	// result. Nothing is readable yet, and nothing is granted.
	CopyStateCopying CopyState = "copying"

	// CopyStateStaged means the transfer finished and delivered the dataset. This is the
	// state a grant is made against.
	CopyStateStaged CopyState = "staged"

	// CopyStateFailed means the transfer did not finish. Distinguishable from staged
	// precisely so that a partial copy can never be granted: the bucket may hold some of
	// the dataset, which is worse than holding none of it, because it looks the same.
	CopyStateFailed CopyState = "failed"

	// CopyStatePurged means deletion of the key encrypting the copy has been scheduled.
	// Once the pending window elapses the objects are ciphertext nobody can read; the
	// copy in the source store is unaffected. Until it elapses the schedule can be
	// cancelled, and a copy whose key decrypts again is staged again.
	CopyStatePurged CopyState = "purged"

	// CopyStateDeleted means the objects themselves have been deleted out of the cache
	// bucket. What that buys is the storage they were occupying, and nothing else:
	// access had already stopped at the purge, and a failed transfer never gave any. The
	// bucket stays, so the dataset can be staged into it again, and the copy in the
	// source store is unaffected.
	CopyStateDeleted CopyState = "deleted"
)

// copyStates lists the states a copy may be persisted in, in lifecycle order.
var copyStates = []CopyState{
	CopyStateCopying,
	CopyStateStaged,
	CopyStateFailed,
	CopyStatePurged,
	CopyStateDeleted,
}

// CopyStates returns the states a copy may be persisted in.
func CopyStates() []CopyState {
	return slices.Clone(copyStates)
}

// IsStored reports whether s is a state a copy may be written in.
func (s CopyState) IsStored() bool {
	return slices.Contains(copyStates, s)
}

func (s CopyState) String() string {
	return string(s)
}

// ParseCopyState converts a persisted state string into a CopyState.
func ParseCopyState(s string) (CopyState, error) {
	candidate := CopyState(s)
	if candidate.IsStored() {
		return candidate, nil
	}
	names := make([]string, 0, len(copyStates))
	for _, state := range copyStates {
		names = append(names, string(state))
	}
	return "", fmt.Errorf("unknown copy state %q; expected one of %s", s, strings.Join(names, ", "))
}

// allowedCopyTransitions lists, for each state, the states a copy may move to.
//
// A copy never returns to an earlier state on account of the transfer: the bytes either
// arrived or they did not, and a second attempt is a second copy with a record of its own
// rather than this one being rewritten. Purge applies from wherever the copy had got to,
// because it schedules destruction of the key the objects are encrypted under.
//
// Deleting the objects comes after that, and only from the two states where nobody is
// reading them: a purged copy, whose key is already on its way out, and a failed one,
// which left part of a dataset behind that was never granted to anybody and never will
// be. A copy still copying or staged is refused, because deleting objects somebody may
// still be reading is the mistake this ordering exists to prevent -- and the way to reach
// the deletion from either of those is to purge first, which is what stops the reading.
//
// Purged is the one state with a way back, and it has one because the thing it records
// has a way back: a scheduled key deletion can be cancelled for as long as its window
// runs, and a copy whose key can decrypt again is a copy that staged. That move is not a
// transfer being retried -- the objects never went anywhere -- so it does not contradict
// the paragraph above. It stops being available once the objects are deleted, which is
// why deleted leads nowhere at all.
var allowedCopyTransitions = map[CopyState][]CopyState{
	CopyStateCopying: {CopyStateStaged, CopyStateFailed, CopyStatePurged},
	CopyStateStaged:  {CopyStatePurged},
	CopyStateFailed:  {CopyStatePurged, CopyStateDeleted},
	CopyStatePurged:  {CopyStateDeleted, CopyStateStaged},
	CopyStateDeleted: nil,
}

// Copy is the ledger record for one transfer of one dataset into this account.
type Copy struct {
	// CopyID identifies this copy. It is minted before the transfer starts, because it is
	// also the folder the copy's artefacts are written to and the name of the execution
	// performing them, so a record can locate a copy that never reported back.
	CopyID string `json:"copyId"`

	// DatasetURI identifies the objects in the source store that were copied as a unit,
	// in the one canonical form, since it is the key every copy of a dataset is found
	// under.
	DatasetURI string `json:"datasetUri"`

	// HotCacheBucket holds this dataset's copy, and KeyArn is the key encrypting that
	// bucket and only that bucket. Both are deployed per dataset, so every copy of one
	// dataset names the same pair; scheduling the key's deletion is what makes a purge
	// irreversible.
	HotCacheBucket string `json:"hotCacheBucket"`
	KeyArn         string `json:"keyArn"`

	// State is the stored lifecycle position.
	State CopyState `json:"state"`

	// CreatedAt is when the transfer was started.
	CreatedAt time.Time `json:"createdAt"`

	// CopyExecutionArn references the execution that performed the transfer, recorded
	// before staging returns so that a copy which never reports back can still be traced.
	CopyExecutionArn string `json:"copyExecutionArn,omitempty"`

	// ObjectCount and TotalBytes describe what actually arrived. Both are zero until the
	// copy reports, and a copy that failed partway reports what it managed.
	ObjectCount int64 `json:"objectCount,omitempty"`
	TotalBytes  int64 `json:"totalBytes,omitempty"`

	// CopySummaryLocation is where the copy's own account of what it transferred was
	// written. Recorded because the execution history does not persist indefinitely, and
	// pointed at the copy kept outside the dataset's own bucket, which is the one that
	// survives a purge.
	CopySummaryLocation string `json:"copySummaryLocation,omitempty"`

	// PurgedAt is when deletion of the key was scheduled, and KeyDeletionDate the moment
	// that deletion becomes irreversible. Both nil until a purge schedules it, and nil
	// again once that schedule is cancelled and the copy staged back: a copy carrying a
	// date its destruction becomes irreversible, while reporting itself grantable, would
	// say two things at once.
	PurgedAt        *time.Time `json:"purgedAt"`
	KeyDeletionDate *time.Time `json:"keyDeletionDate"`

	// DeletedAt is when the objects were deleted out of the cache bucket, and nil until
	// they are. It has no companion date because there is no window on it: unlike a
	// scheduled key deletion, which can be cancelled until its date passes, an object
	// that has been deleted is gone the moment it is.
	DeletedAt *time.Time `json:"deletedAt"`
}

// IsGrantable reports whether this copy delivered a dataset a grant can be made against.
func (c *Copy) IsGrantable() bool {
	return c.State == CopyStateStaged
}

// IsDeletable reports whether the objects this copy staged may be deleted out of the
// cache bucket, which is true only where nobody could be reading them: the lifecycle
// already says which states those are by permitting the move, so this asks it rather than
// naming them a second time.
//
// A copy whose objects are already gone answers false, because there is nothing left to
// delete rather than because deleting would be wrong.
func (c *Copy) IsDeletable() bool {
	return slices.Contains(allowedCopyTransitions[c.State], CopyStateDeleted)
}

// IsDestroyed reports whether the pending window on a purged copy's key has already
// elapsed, which is the point past which the objects are unreadable for good.
//
// A copy whose objects have since been deleted still answers by the key, because the key
// is what the callers of this ask about: the deletion is a later step that reclaims the
// storage the unreadable ciphertext was occupying, and a copy deleted out of the failed
// state -- one that was never purged, and so has no deletion date -- is not destroyed in
// this sense at all.
func (c *Copy) IsDestroyed(now time.Time) bool {
	return (c.State == CopyStatePurged || c.State == CopyStateDeleted) &&
		c.KeyDeletionDate != nil &&
		!c.KeyDeletionDate.After(now)
}
