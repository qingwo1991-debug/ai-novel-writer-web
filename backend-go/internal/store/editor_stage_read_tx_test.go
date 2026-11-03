package store

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

func TestEditorStageTransactionReadsMatchExistingRepositories(t *testing.T) {
	ctx := context.Background()
	coreDB, coreRepository, _ := projectCoreFixture(t, false)
	coreTx, err := coreDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer coreTx.Rollback()
	wantCore, err := coreRepository.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotCore, err := ReadProjectCoreTx(ctx, coreTx)
	if err != nil || !reflect.DeepEqual(gotCore, wantCore) {
		t.Fatalf("core = %#v, %v; want %#v", gotCore, err, wantCore)
	}

	blueprintDB, blueprintRepository, _ := blueprintFixture(t)
	if err := blueprintRepository.Upsert(ctx, blueprintData(777)); err != nil {
		t.Fatal(err)
	}
	blueprintTx, err := blueprintDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer blueprintTx.Rollback()
	wantBlueprints, err := blueprintRepository.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotBlueprints, err := ReadBlueprintsTx(ctx, blueprintTx)
	if err != nil || !reflect.DeepEqual(gotBlueprints, wantBlueprints) {
		t.Fatalf("blueprints = %#v, %v; want %#v", gotBlueprints, err, wantBlueprints)
	}
	if _, err := ReadProjectCoreTx(ctx, nil); err == nil {
		t.Fatal("nil transaction accepted")
	}
	if _, err := ReadBlueprintsTx(ctx, nil); err == nil {
		t.Fatal("nil transaction accepted")
	}
}
