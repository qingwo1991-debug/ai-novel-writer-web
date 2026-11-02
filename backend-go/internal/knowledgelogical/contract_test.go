package knowledgelogical

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

func testBudget() knowledgedomain.Budget { return knowledgedomain.Budget{MaxTables:32,MaxRows:10000,MaxCells:200000,MaxTextBytes:32<<20} }

func TestActualExportedOriginalModuleContract(t *testing.T) {
	path := os.Getenv("GO_KNOWLEDGE_LOGICAL_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" { t.Fatal("original logical contract mandatory") }
		t.Skip("CI-only actual exported-module oracle")
	}
	raw, err := os.ReadFile(path); if err != nil { t.Fatal(err) }
	var contract struct {
		Format int `json:"format"`
		Locale string `json:"locale"`
		Oracle string `json:"oracle"`
		Cases []struct {
			Name string `json:"name"`
			Snapshot knowledgedomain.Snapshot `json:"snapshot"`
			RegistryRaw *string `json:"registryRaw"`
			GoAccepted bool `json:"goAccepted"`
			Summary Summary `json:"summary"`
			Tables []TableHash `json:"tables"`
		} `json:"cases"`
		Portable []struct {
			Name string `json:"name"`
			Value struct {
				Version int `json:"version"`
				Documents []struct {
					DocID string `json:"docId"`
					FileName string `json:"fileName"`
					CorpusKind string `json:"corpusKind"`
					Chunks []struct { ChunkIndex any `json:"chunkIndex"`; Text string `json:"text"` } `json:"chunks"`
					Copy *struct { Content string `json:"content"`; IndexedHash string `json:"indexedHash"`; Edited bool `json:"edited"`; IndexDirty bool `json:"indexDirty"` } `json:"copy"`
				} `json:"documents"`
			} `json:"value"`
			OriginalAccepted bool `json:"originalAccepted"`
			GoAccepted bool `json:"goAccepted"`
			Bytes *string `json:"bytes"`
		} `json:"portable"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw)); decoder.UseNumber()
	if err := decoder.Decode(&contract); err != nil { t.Fatal(err) }
	if contract.Format != 1 || contract.Locale != "en-US" || contract.Oracle != "whole-original-exported-modules-real-Arrow-table-only-Lance-seam" || len(contract.Cases) < 20 || len(contract.Portable) < 20 { t.Fatal("invalid contract identity/coverage") }
	for _, item := range contract.Cases { t.Run(item.Name,func(t *testing.T) {
		if item.RegistryRaw == nil { item.Snapshot.Registry = nil } else { item.Snapshot.Registry = json.RawMessage(*item.RegistryRaw) }
		before, _ := json.Marshal(item.Snapshot)
		actual, err := HashSnapshot(context.Background(),item.Snapshot,testBudget(),Options{Locale:contract.Locale})
		if (err == nil) != item.GoAccepted { t.Fatalf("goAccepted=%t err=%v",item.GoAccepted,err) }
		if err != nil { if !reflect.DeepEqual(actual,Summary{}) { t.Fatal("nonzero error result") }; return }
		if actual.LogicalHash != item.Summary.LogicalHash || actual.Documents != item.Summary.Documents || actual.Chunks != item.Summary.Chunks || actual.TableCount != item.Summary.TableCount { t.Fatalf("original summary mismatch: %+v %+v",actual,item.Summary) }
		for _, expected := range item.Tables { found:=false; for _, table := range actual.Tables { if expected.Name == table.Name { found=true; if table.LogicalHash != expected.LogicalHash { t.Fatalf("Arrow schema/record hash mismatch for %s",table.Name) } } }; if !found { t.Fatal("missing table") } }
		after, _ := json.Marshal(item.Snapshot); if !bytes.Equal(before,after) { t.Fatal("caller snapshot mutated") }
	}) }
	for _, item := range contract.Portable { t.Run("portable-"+item.Name,func(t *testing.T) {
		snapshot:=PortableSnapshot{Version:item.Value.Version}
		for _, source := range item.Value.Documents {
			doc:=PortableDocument{DocID:source.DocID,FileName:source.FileName,CorpusKind:source.CorpusKind}
			for _, chunk := range source.Chunks { n, err:=number(chunk.ChunkIndex); if err != nil { t.Fatal(err) }; doc.Chunks=append(doc.Chunks,PortableChunk{ChunkIndex:int(n),Text:chunk.Text}) }
			if copy:=source.Copy; copy!=nil { doc.Copy=&knowledgecopy.Copy{Content:copy.Content,IndexedHash:copy.IndexedHash,Edited:copy.Edited,IndexDirty:copy.IndexDirty} }
			snapshot.Documents=append(snapshot.Documents,doc)
		}
		actual,err:=SerializePortable(context.Background(),snapshot,testBudget(),Options{Locale:contract.Locale})
		if (err == nil) != item.GoAccepted { t.Fatalf("originalAccepted=%t goAccepted=%t err=%v",item.OriginalAccepted,item.GoAccepted,err) }
		if err != nil { if actual != nil { t.Fatal("bytes leaked on error") }; return }
		if item.Bytes == nil || string(actual) != *item.Bytes { t.Fatal("original portable bytes mismatch") }
	}) }
}
