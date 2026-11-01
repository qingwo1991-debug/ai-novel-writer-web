// A navigation/close hint is not an unconditional cancellation command. A
// refresh may reconnect the same tab while the old document's keepalive POST is
// still in flight. Preserve the worker sink until no connection/request/stream
// remains, and ignore callbacks invalidated by a reconnect.
export class ClientDepartures {
  constructor({ release, busy, delayMs = 1500, schedule = setTimeout, cancel = clearTimeout }) {
    this.release = release; this.busy = busy; this.delayMs = delayMs
    this.schedule = schedule; this.cancel = cancel; this.pending = new Map()
  }
  connect(client) {
    const pending = this.pending.get(client)
    if (pending?.timer) this.cancel(pending.timer)
    this.pending.delete(client)
  }
  depart(client) {
    if (this.pending.has(client)) return
    const pending = { settled: false }
    this.pending.set(client, pending)
    pending.timer = this.schedule(() => {
      if (this.pending.get(client) !== pending) return
      pending.settled = true; pending.timer = undefined; this.check(client)
    }, this.delayMs)
    pending.timer?.unref?.()
  }
  check(client) {
    const pending = this.pending.get(client)
    if (!pending?.settled) return
    // A late old-document hint must not linger until a future network outage.
    if (client.connections.size) { this.pending.delete(client); return }
    if (this.busy(client)) return
    this.pending.delete(client); this.release(client)
  }
  forget(client) { this.connect(client) }
  close() { for (const client of this.pending.keys()) this.forget(client) }
}
