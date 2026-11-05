//go:build linux

package main

import (
	"context"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's thin adapter from the durable author
// admission to the three transport providers the continuity, post-process and
// plot-tree binders borrow: transport.SummaryProvider (BeginSummary),
// transport.PostHistoryProvider (BeginPostHistory) and
// transport.PlotTreeProvider (BeginPlotTree).
//
// All three go through borrowAuthorDatabase, which is the identical fence the
// draft CAS, the character reads, the editor stage reads and the blueprint read
// already use: the request's frozen three-key context is parsed, the caller's own
// cookie/tab identity is named, the project path must be one the startup
// inventory issued a catalog key for, and authorruntime re-proves the lease, the
// admission permit and the handle identity before lending the author's own
// database. Nothing here opens a database, resolves a path, or makes an admission
// decision, and no repository is constructed over a database that failed the
// pinned v7 admission.
//
// store.NewSummary(db) is exactly the repository
// authorruntime.ReadLifecycle.SummaryRepositoryDurable
// (internal/authorruntime/author_repositories_linux.go:24) builds on the same
// handle; the borrow's release closure carries that path's refresh-then-done
// fence unchanged, so a read commits nothing and its refresh is only a
// re-verification.

// BeginSummary lends the continuity and legacy summary snapshot repository.
// BindSummary installs the three SELECT-only reads (latest snapshot, the
// before-chapter list, the finalized source projection), the one legacy snapshot
// INSERT, and the two continuity names that binder refuses unconditionally.
func (d *productionDurableOpen) BeginSummary(ctx context.Context, caller transport.Caller, raw any) (*store.Summary, func(), error) {
	db, release, err := d.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewSummary(db), release, nil
}

// BeginPostHistory lends the post-process receipt and LLM ledger repository for
// the five reads BindPostHistoryReads installs. Its four mutating names
// (create-run, mark-step-ok, mark-step-failed, log-llm-call) stay unbound, so
// this borrow is never reached by a write on this lane.
func (d *productionDurableOpen) BeginPostHistory(ctx context.Context, caller transport.Caller, raw any) (*store.PostHistory, func(), error) {
	db, release, err := d.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewPostHistory(db), release, nil
}

// BeginPlotTree lends the plot tree repository for the single read
// BindPlotTreeReads installs. store.PlotTree.Read opens its own read-only
// transaction and never writes, so this borrow cannot replace the stored
// snapshot; db:plot-tree-save and db:plot-tree-clear stay unbound here.
func (d *productionDurableOpen) BeginPlotTree(ctx context.Context, caller transport.Caller, raw any) (*store.PlotTree, func(), error) {
	db, release, err := d.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewPlotTree(db), release, nil
}
