package runtime

import (
 "errors"
 "sync"
 "testing"
)
func TestSameTabRefreshRetainsLeaseAndOtherTabConflicts(t *testing.T) {
 leases:=NewLeases(); first,fresh,err:=leases.Acquire("/synthetic/book","project-1","session-a:tab-1")
 if err!=nil || !fresh { t.Fatal(first,fresh,err) }
 reopened,fresh,err:=leases.Acquire(first.ProjectPath,first.ProjectID,first.Owner)
 if err!=nil || fresh || reopened!=first {t.Fatal("refresh rotated lease",reopened,fresh,err)}
 if _,_,err=leases.Acquire(first.ProjectPath,first.ProjectID,"session-a:tab-2");!errors.Is(err,ErrOwned){t.Fatal(err)}
 if _,_,err=leases.Acquire(first.ProjectPath,first.ProjectID,"session-b:tab-1");!errors.Is(err,ErrOwned){t.Fatal(err)}
}
func TestLeaseFencesReleaseAndStaleOperation(t *testing.T) {
 leases:=NewLeases(); first,_,_:=leases.Acquire("/synthetic/book","project-1","session:tab")
 finish,err:=leases.Begin(first);if err!=nil{t.Fatal(err)}
 if err=leases.Release(first);!errors.Is(err,ErrBusy){t.Fatal(err)}
 finish();finish();if err=leases.Release(first);err!=nil{t.Fatal(err)}
 next,_,_:=leases.Acquire(first.ProjectPath,first.ProjectID,first.Owner)
 if next.LeaseID==first.LeaseID{t.Fatal("expired lease reused")}
 if _,err=leases.Begin(first);!errors.Is(err,ErrNotOwned){t.Fatal(err)}
 if err=leases.Release(first);!errors.Is(err,ErrNotOwned){t.Fatal(err)}
}
func TestFailedOpenOnlyRollsBackNewOwner(t *testing.T){
 leases:=NewLeases(); first,fresh,_:=leases.Acquire("/synthetic/book","id","owner")
 if err:=leases.RollbackAcquire(first,fresh);err!=nil{t.Fatal(err)}
 first,_,_=leases.Acquire("/synthetic/book","id","owner")
 reopened,fresh,_:=leases.Acquire(first.ProjectPath,first.ProjectID,first.Owner)
 if err:=leases.RollbackAcquire(reopened,fresh);err!=nil{t.Fatal(err)}
 if _,_,err:=leases.Acquire(first.ProjectPath,first.ProjectID,"other");!errors.Is(err,ErrOwned){t.Fatal("existing owner lost",err)}
}
func TestConcurrentAcquireHasSingleWinner(t *testing.T){
 leases:=NewLeases(); var wg sync.WaitGroup; var mu sync.Mutex; wins:=0
 for range 32 {wg.Go(func(){owner,_:=token();_,fresh,err:=leases.Acquire("/synthetic/book","id",owner);if err==nil&&fresh{mu.Lock();wins++;mu.Unlock()}else if !errors.Is(err,ErrOwned){t.Errorf("unexpected %v",err)}})}
 wg.Wait();if wins!=1{t.Fatal("winners",wins)}
}
func TestCommittedSameOwnerOpenSurvivesFreshOpenerRollback(t *testing.T){
 leases:=NewLeases()
 first,fresh,err:=leases.Acquire("/synthetic/book","id","owner");if err!=nil||!fresh{t.Fatal(first,fresh,err)}
 reused,reusedFresh,err:=leases.Acquire(first.ProjectPath,first.ProjectID,first.Owner);if err!=nil||reusedFresh{t.Fatal(reused,reusedFresh,err)}
 // A second opener succeeds before the original fresh opener fails.
 if err:=leases.CommitAcquire(reused);err!=nil{t.Fatal(err)}
 if err:=leases.CommitAcquire(reused);err!=nil{t.Fatal("commit not idempotent",err)}
 if err:=leases.RollbackAcquire(first,fresh);err!=nil{t.Fatal(err)}
 if _,_,err:=leases.Acquire(first.ProjectPath,first.ProjectID,"other");!errors.Is(err,ErrOwned){t.Fatal("successful opener lost ownership",err)}
 finish,err:=leases.Begin(reused);if err!=nil{t.Fatal(err)}
 if err:=leases.Release(reused);!errors.Is(err,ErrBusy){t.Fatal("commit bypassed active fence",err)}
 finish();if err:=leases.Release(reused);err!=nil{t.Fatal(err)}
}
func TestRolledBackAcquisitionCannotCommitOrAffectReplacement(t *testing.T){
 leases:=NewLeases()
 first,fresh,err:=leases.Acquire("/synthetic/book","id","owner");if err!=nil||!fresh{t.Fatal(first,fresh,err)}
 reused,_,err:=leases.Acquire(first.ProjectPath,first.ProjectID,first.Owner);if err!=nil{t.Fatal(err)}
 if err:=leases.RollbackAcquire(first,fresh);err!=nil{t.Fatal(err)}
 replacement,_,err:=leases.Acquire(first.ProjectPath,first.ProjectID,"other");if err!=nil{t.Fatal(err)}
 if err:=leases.CommitAcquire(reused);!errors.Is(err,ErrNotOwned){t.Fatal("stale opener committed",err)}
 if err:=leases.RollbackAcquire(first,fresh);!errors.Is(err,ErrNotOwned){t.Fatal("stale rollback accepted",err)}
 if err:=leases.CommitAcquire(replacement);err!=nil{t.Fatal("replacement affected",err)}
}
func TestConcurrentCommitAndRollbackAreAtomic(t *testing.T){
 for range 64 {
  leases:=NewLeases();lease,fresh,err:=leases.Acquire("/synthetic/book","id","owner");if err!=nil{t.Fatal(err)}
  start:=make(chan struct{});committed:=make(chan error,1);rolledBack:=make(chan error,1)
  go func(){<-start;committed<-leases.CommitAcquire(lease)}()
  go func(){<-start;rolledBack<-leases.RollbackAcquire(lease,fresh)}()
  close(start);commitErr,rollbackErr:=<-committed,<-rolledBack
  if rollbackErr!=nil{t.Fatal("rollback failed",rollbackErr)}
  next,nextFresh,err:=leases.Acquire(lease.ProjectPath,lease.ProjectID,lease.Owner);if err!=nil{t.Fatal(err)}
  switch {
  case commitErr==nil:
   if nextFresh||next!=lease{t.Fatal("successful commit revoked",next,nextFresh)}
  case errors.Is(commitErr,ErrNotOwned):
   if !nextFresh||next.LeaseID==lease.LeaseID{t.Fatal("rolled-back acquisition retained",next,nextFresh)}
  default:t.Fatal("unexpected commit error",commitErr)
  }
 }
}
func TestIndependentProjectsAndIdentityReplacement(t *testing.T){
 leases:=NewLeases();_,_,err:=leases.Acquire("/synthetic/a","id-a","owner");if err!=nil{t.Fatal(err)}
 if _,_,err=leases.Acquire("/synthetic/b","id-b","another");err!=nil{t.Fatal(err)}
 if _,_,err=leases.Acquire("/synthetic/a","replaced-id","owner");!errors.Is(err,ErrNotOwned){t.Fatal(err)}
}
