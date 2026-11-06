//go:build linux

package main

// This file proves the success path of llm:close-execution-lease. Until now the
// only assertions on the name were that it is bound
// (production_llm_execution_linux_test.go:479-489) and that the synthetic lane
// expects GO_OPERATION_NOT_MIGRATED for it, so neither the ctx.Err() early return
// nor the leases.Close == false branch of x.close
// (production_llm_execution_linux.go:283-301) had ever run, and no test ever saw
// the {"success":true} answer or asked what a closed lease may still do.
//
// The grammar these tests hold the server to is the desktop controller's:
//
//	electron/controllers/llm-controller.ts:213-225
//	  if (modelExecutionLeases.close(leaseId)) { ...tombstone...; return { success: true } }
//	  if (closedExecutionLeaseTombstones.has(leaseId)) return { success: true }
//	  return { success: false, error: 'MODEL_EXECUTION_LEASE_INVALID' }
//
// A close therefore releases authority, and a second close of a lease this tab
// already closed is the renderer's already-closed case: idempotent success, never
// the refusal the generation runtime turns into LEASE_CLOSE_FAILED
// (src/services/generation/generation-runtime.ts:404-410).
//
// Everything runs over real HTTP through the frozen session fixture the
// neighbouring tests use. The stream half is proved by the provider observing the
// end of its own request on a channel, not by a field this server could have
// flipped, and the silence after the run settles is the only claim a timed window
// makes.

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// productionLLMCloseTestStreamRequest is the identity of the one stream this
	// file closes a lease under.
	productionLLMCloseTestStreamRequest = "prod-llm-close-stream-1"
	// productionLLMCloseTestWait bounds one fact that settles on another goroutine.
	// The failure messages name the fact.
	productionLLMCloseTestWait = 5 * time.Second
	// productionLLMCloseTestSilence is the window in which no frame may arrive after
	// the run settled, which is a claim only a window can make.
	productionLLMCloseTestSilence = 250 * time.Millisecond
)

// productionLLMCloseWire is the {success, error} answer of
// llm:close-execution-lease (src/shared/ipc-channels.ts:552-555).
type productionLLMCloseWire struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// productionLLMCloseLease sends llm:close-execution-lease over HTTP and decodes the
// answer the browser would receive.
func productionLLMCloseLease(t *testing.T, client *productionDurableClient, leaseID string) productionLLMCloseWire {
	t.Helper()
	var answer productionLLMCloseWire
	client.decode(t, "llm:close-execution-lease", []any{leaseID}, &answer)
	return answer
}

// productionLLMCloseStubStream answers one delta and then holds the socket open
// until the request context ends, reporting that end on a channel. A channel makes
// "the provider's own call was stopped" a fact the test waits for instead of a fact
// it sleeps and hopes for.
func productionLLMCloseStubStream(delta string, stopped chan<- struct{}) func(*productionLLMStub, http.ResponseWriter, *http.Request) {
	return func(_ *productionLLMStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":"+
			productionLLMJSONString(delta)+"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(stopped)
	}
}

// TestLLMCloseExecutionLeaseReleasesTheLeaseAndRefusesItsReuse is the success path:
// a began lease is closed, the registry drops it, and the same id can authorise
// nothing afterwards - neither a generate nor a stream - and nothing is dialed.
func TestLLMCloseExecutionLeaseReleasesTheLeaseAndRefusesItsReuse(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	// A provider that would answer text if the closed lease still carried authority.
	fixture.provider.serve = productionLLMStubCompletion("不该出现")
	client := fixture.client(t, productionTestTab)
	receipt := fixture.lease(t, client)
	if outstanding := fixture.execution.leases.Outstanding(); outstanding != 1 {
		t.Fatalf("beginning one lease left %d leases outstanding", outstanding)
	}

	answer := productionLLMCloseLease(t, client, receipt.LeaseID)
	if !answer.Success || answer.Error != "" {
		t.Fatalf("llm:close-execution-lease refused a lease this tab holds: success=%v error=%v", answer.Success, answer.Error)
	}
	if outstanding := fixture.execution.leases.Outstanding(); outstanding != 0 {
		t.Fatalf("closing the lease left %d leases outstanding", outstanding)
	}

	refused := fixture.generate(t, client, productionLLMGenerateRequest(receipt.LeaseID))
	if refused.Success || refused.Error != "MODEL_EXECUTION_LEASE_INVALID" {
		t.Fatalf("a closed lease still generated: success=%v error=%v", refused.Success, refused.Error)
	}
	if refused.Content != "" {
		t.Fatal("a refused request carried model text")
	}

	var stream struct {
		RequestID string `json:"requestId"`
		Started   bool   `json:"started"`
		Error     string `json:"error"`
	}
	client.decode(t, "llm:generate-stream", []any{productionLLMCloseTestStreamRequest,
		productionLLMGenerateRequest(receipt.LeaseID)}, &stream)
	if stream.Started || stream.Error != "MODEL_EXECUTION_LEASE_INVALID" {
		t.Fatalf("a closed lease still started a stream: started=%v error=%v", stream.Started, stream.Error)
	}
	if fixture.provider.count() != 0 {
		t.Fatalf("a closed lease reached the provider %d times", fixture.provider.count())
	}
	if runs := productionLLMStreamCount(fixture.execution); runs != 0 {
		t.Fatalf("a refused stream was registered anyway: %d runs", runs)
	}
}

// TestLLMCloseExecutionLeaseStopsTheStreamItAuthorised proves the close reaches the
// run the lease authorised: the provider's own call ends, the run settles with the
// one terminal frame the renderer's grammar requires, and no further frame arrives.
func TestLLMCloseExecutionLeaseStopsTheStreamItAuthorised(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	stopped := make(chan struct{})
	fixture.provider.serve = productionLLMCloseStubStream("Hel", stopped)
	client := fixture.client(t, productionTestTab)
	receipt := fixture.lease(t, client)
	subscription := fixture.subscribe(t, client)

	var started struct {
		RequestID string `json:"requestId"`
		Started   bool   `json:"started"`
		Error     string `json:"error"`
	}
	client.decode(t, "llm:generate-stream", []any{productionLLMCloseTestStreamRequest,
		productionLLMGenerateRequest(receipt.LeaseID)}, &started)
	if !started.Started || started.RequestID != productionLLMCloseTestStreamRequest {
		t.Fatalf("the stream did not start: started=%v error=%v", started.Started, started.Error)
	}
	channel, chunk := productionLLMFrame(t, subscription.next(t))
	if channel != "llm:stream-chunk" || chunk["chunk"] != "Hel" {
		t.Fatalf("the first frame is not the delta the provider sent: %v %v", channel, chunk["chunk"])
	}
	if runs := productionLLMStreamCount(fixture.execution); runs != 1 {
		t.Fatalf("%d streaming runs are registered while the provider is still holding the socket", runs)
	}

	answer := productionLLMCloseLease(t, client, receipt.LeaseID)
	if !answer.Success {
		t.Fatalf("llm:close-execution-lease refused the lease the live run is spending: error=%v", answer.Error)
	}

	// The run was authorised by the lease that just closed, so it stops: exactly one
	// terminal frame, the cancelled settlement.
	channel, settled := productionLLMFrame(t, subscription.next(t))
	if channel != "llm:stream-done" {
		t.Fatalf("a run closed under reported %v instead of llm:stream-done", channel)
	}
	if settled["finishReason"] != "cancelled" {
		t.Fatalf("a run closed under settled as %v instead of cancelled", settled["finishReason"])
	}
	if settled["fullText"] != "Hel" {
		t.Fatalf("a run closed under lost the delta it had already published: %v", settled["fullText"])
	}

	// The proof that the stream goroutine stopped rather than a flag flipping: the
	// provider itself observes the end of the request only that goroutine owned. If
	// the close left it running, this blocks until the bounded window closes.
	select {
	case <-stopped:
	case <-time.After(productionLLMCloseTestWait):
		t.Fatal("waited for the provider to observe the end of its request after the lease closed: the stream goroutine was still running")
	}
	if fixture.provider.count() != 1 {
		t.Fatalf("the stopped run dialed the provider %d times", fixture.provider.count())
	}

	// And nothing may follow the one terminal frame.
	subscription.quiet(t, productionLLMCloseTestSilence)
}

// TestLLMCloseExecutionLeaseTwiceIsIdempotent holds the server to the desktop's own
// closed-lease grammar (electron/controllers/llm-controller.ts:223):
//
//	if (closedExecutionLeaseTombstones.has(leaseId)) return { success: true }
//
// A second close of a lease this tab already closed is therefore success. The
// renderer turns success:false into LEASE_CLOSE_FAILED
// (src/services/generation/generation-runtime.ts:404-410) and shows it to the
// author, so a refusal here is a failure reported for an operation that already
// succeeded.
func TestLLMCloseExecutionLeaseTwiceIsIdempotent(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	fixture.provider.serve = productionLLMStubCompletion("租约还在")
	client := fixture.client(t, productionTestTab)
	receipt := fixture.lease(t, client)
	// A second, unrelated lease stays open, so "the tombstone touched nothing" is
	// observable: a close that reached the registry again would release it too.
	other := fixture.lease(t, client)
	if outstanding := fixture.execution.leases.Outstanding(); outstanding != 2 {
		t.Fatalf("two begun leases left %d outstanding", outstanding)
	}

	first := productionLLMCloseLease(t, client, receipt.LeaseID)
	if !first.Success || first.Error != "" {
		t.Fatalf("the first close refused a lease this tab holds: success=%v error=%v", first.Success, first.Error)
	}
	second := productionLLMCloseLease(t, client, receipt.LeaseID)
	if !second.Success || second.Error != "" {
		t.Fatalf("closing an already-closed lease answered success=%v error=%v, and the renderer reports that as LEASE_CLOSE_FAILED", second.Success, second.Error)
	}
	// Idempotence must not resurrect it and must not release anything else.
	if outstanding := fixture.execution.leases.Outstanding(); outstanding != 1 {
		t.Fatalf("%d leases are outstanding after closing one lease twice, want the one other lease", outstanding)
	}
	refused := fixture.generate(t, client, productionLLMGenerateRequest(receipt.LeaseID))
	if refused.Success || refused.Error != "MODEL_EXECUTION_LEASE_INVALID" {
		t.Fatalf("a twice-closed lease generated again: success=%v error=%v", refused.Success, refused.Error)
	}
	if survivor := fixture.generate(t, client, productionLLMGenerateRequest(other.LeaseID)); !survivor.Success {
		t.Fatalf("a repeated close released the other lease too: %v", survivor.Error)
	}
}

// TestLLMCloseExecutionLeaseRefusesAnIdItNeverMinted proves the tombstone cannot be
// widened: the renderer's own rule (electron/controllers/llm-controller.ts:223) is
// "a lease this process closed answers success", and the line after it refuses every
// other id. An id no begin ever minted has no tombstone, so it is refused, and it
// stays refused on a second attempt instead of becoming a tombstone of itself.
func TestLLMCloseExecutionLeaseRefusesAnIdItNeverMinted(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	client := fixture.client(t, productionTestTab)
	// A well-formed id of the shape begin mints, so only the registry can refuse it.
	const neverMinted = "0f5b0b7e-9a5d-4c07-8f2e-3c1d0a5b6e74"

	for attempt := 0; attempt < 2; attempt++ {
		answer := productionLLMCloseLease(t, client, neverMinted)
		if answer.Success || answer.Error != "MODEL_EXECUTION_LEASE_INVALID" {
			t.Fatalf("close attempt %d of a lease this process never minted answered success=%v error=%v", attempt+1, answer.Success, answer.Error)
		}
	}
	if outstanding := fixture.execution.leases.Outstanding(); outstanding != 0 {
		t.Fatalf("closing an id that was never minted left %d leases outstanding", outstanding)
	}
}

// TestLLMCloseExecutionLeaseWithADeadRequestContextReleasesNothing exercises the
// ctx.Err() early return (production_llm_execution_linux.go:288-290), which no HTTP
// call can reach: the transport always hands a handler a live context. The handler is
// therefore called directly with a cancelled one, and the lease is then spent over
// real HTTP, so what is asserted is the effect the branch must have - none.
func TestLLMCloseExecutionLeaseWithADeadRequestContextReleasesNothing(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	fixture.provider.serve = productionLLMStubCompletion("租约还在")
	client := fixture.client(t, productionTestTab)
	receipt := fixture.lease(t, client)

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	// The owner is exactly the one that minted the lease: the session cookie the
	// durable client bootstrapped with is the SessionID the transport hands every
	// handler (internal/transport/drafts_test.go:117 builds a Caller the same way), so
	// the only thing between this call and a released lease is the branch under test.
	answer, err := fixture.execution.close(dead, transport.Caller{
		Principal: productionLLMTestPrincipal,
		SessionID: client.cookie.Value,
		ClientID:  productionTestTab,
	}, []any{receipt.LeaseID})
	if err != nil {
		t.Fatalf("close over a dead request context answered an HTTP error: %v", err)
	}
	refusal, ok := answer.(map[string]any)
	if !ok {
		t.Fatal("close over a dead request context did not answer a result object")
	}
	if success, _ := refusal["success"].(bool); success {
		t.Fatal("close over a dead request context reported success")
	}
	if code, _ := refusal["error"].(string); code != "MODEL_EXECUTION_LEASE_INVALID" {
		t.Fatalf("close over a dead request context answered error %v", code)
	}
	if outstanding := fixture.execution.leases.Outstanding(); outstanding != 1 {
		t.Fatalf("a close over a dead request context released the lease: %d outstanding", outstanding)
	}
	// The lease is still the tab's to spend, which is what "released nothing" means.
	generated := fixture.generate(t, client, productionLLMGenerateRequest(receipt.LeaseID))
	if !generated.Success || generated.Content != "租约还在" {
		t.Fatalf("the lease a dead-context close was refused for stopped working: success=%v error=%v", generated.Success, generated.Error)
	}
}
