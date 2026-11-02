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
func TestIndependentProjectsAndIdentityReplacement(t *testing.T){
 leases:=NewLeases();_,_,err:=leases.Acquire("/synthetic/a","id-a","owner");if err!=nil{t.Fatal(err)}
 if _,_,err=leases.Acquire("/synthetic/b","id-b","another");err!=nil{t.Fatal(err)}
 if _,_,err=leases.Acquire("/synthetic/a","replaced-id","owner");!errors.Is(err,ErrNotOwned){t.Fatal(err)}
}
