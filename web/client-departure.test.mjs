import { test } from 'node:test'
import assert from 'node:assert/strict'
import { ClientDepartures } from './client-departure.mjs'

function fixture() {
  let callback, busy = false, releases = 0, cancelled = 0
  const client = { connections: new Set() }
  const departures = new ClientDepartures({ release(value) { assert.equal(value, client); releases++ }, busy: () => busy,
    schedule(fn, delay) { assert.equal(delay, 1500); callback = fn; return { unref() {} } }, cancel() { cancelled++ } })
  return { client, departures, settle: () => callback(), setBusy: value => { busy = value }, releases: () => releases, cancelled: () => cancelled }
}
test('idle page departure releases only after grace and SSE disconnection', () => {
  const f = fixture(); f.departures.depart(f.client); assert.equal(f.releases(), 0)
  f.settle(); assert.equal(f.releases(), 1)
  f.departures.check(f.client); assert.equal(f.releases(), 1)
})
test('same-tab refresh reconnect invalidates pending release, even if timer callback was already queued', () => {
  const f = fixture(); f.departures.depart(f.client); f.departures.connect(f.client)
  f.settle(); assert.equal(f.releases(), 0); assert.equal(f.cancelled(), 1)
})
test('late old-document departure cannot release an already connected replacement document', () => {
  const f = fixture(); f.client.connections.add('new-document'); f.departures.depart(f.client); f.settle()
  assert.equal(f.releases(), 0); f.client.connections.clear(); f.departures.check(f.client); assert.equal(f.releases(), 0)
})
test('active provider request or stream is never cancelled; releases only when effect settles', () => {
  const f = fixture(); f.setBusy(true); f.departures.depart(f.client); f.settle()
  assert.equal(f.releases(), 0); f.departures.check(f.client); assert.equal(f.releases(), 0)
  f.setBusy(false); f.departures.check(f.client); assert.equal(f.releases(), 1)
})
test('reconnect while provider request runs preserves sink after request settles', () => {
  const f = fixture(); f.setBusy(true); f.departures.depart(f.client); f.settle(); f.departures.connect(f.client)
  f.setBusy(false); f.departures.check(f.client); assert.equal(f.releases(), 0)
})
test('shutdown and client expiry discard timers without extra release', () => {
  for (const method of ['close', 'forget']) { const f = fixture(); f.departures.depart(f.client); f.departures[method](f.client); f.settle(); assert.equal(f.releases(), 0) }
})
