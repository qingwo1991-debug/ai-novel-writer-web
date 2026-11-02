package runtime

import (
 "errors"
 "testing"
)
func TestReleaseAllAtomicallyRefusesActiveProjects(t *testing.T){
 leases:=NewLeases();a,_,err:=leases.Acquire("/synthetic/a","a","owner-a");if err!=nil{t.Fatal(err)};b,_,err:=leases.Acquire("/synthetic/b","b","owner-b");if err!=nil{t.Fatal(err)}
 done,err:=leases.Begin(b);if err!=nil{t.Fatal(err)}
 if err:=leases.ReleaseAll();!errors.Is(err,ErrBusy){t.Fatal(err)}
 // Refusal must leave every lease, including idle projects, unchanged.
 finish,err:=leases.Begin(a);if err!=nil{t.Fatal("idle lease partially released",err)};finish();done()
 if err:=leases.ReleaseAll();err!=nil{t.Fatal(err)}
 if _,err:=leases.Begin(a);!errors.Is(err,ErrNotOwned){t.Fatal(err)};if _,err:=leases.Begin(b);!errors.Is(err,ErrNotOwned){t.Fatal(err)}
 if err:=leases.ReleaseAll();err!=nil{t.Fatal("idempotent",err)}
}
