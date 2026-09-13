package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
)

// memoryOrderDispatcher is the controller's live order dispatcher and the
// concrete implementation of the reusable orderdispatch.Dispatcher seam. The
// webhook receiver (E3/E6) fires orders through this same instance, so a webhook
// dispatch and a tick dispatch run the identical dispatchOne core.
var _ orderdispatch.Dispatcher = (*memoryOrderDispatcher)(nil)

// Dispatch fires a pre-resolved order through the shared launchResolvedDispatch
// → dispatchOne core — the same path the controller tick loop uses.
//
// The caller (the webhook order sink) has already enforced its policy:
// target=="order", the order opts in with trigger=="webhook", the {order,rig}
// is within the webhook's provenance scope, and untrusted payload args have been
// namespaced into req.ExecEnv (R4). Dispatch therefore only re-checks required
// params (defense in depth), resolves the order's store, and launches the async
// dispatch. It returns as soon as the tracking bead is written and the goroutine
// is launched; the wisp/exec runs asynchronously and its outcome lands on the
// events feed (OrderFired/Completed/Failed), matching the design's fast-ACK
// response contract (verify+match+enqueue synchronously, work runs async).
func (m *memoryOrderDispatcher) Dispatch(ctx context.Context, req orderdispatch.DispatchRequest) (orderdispatch.DispatchResult, error) {
	a := req.Order
	scoped := a.ScopedName()

	// Defense in depth: the sink validated already, but the seam is the last
	// gate before a bead is written, so re-check against the raw param vars.
	if err := orders.ValidateRequiredParams(a, req.Vars); err != nil {
		return orderdispatch.DispatchResult{ScopedName: scoped, Rejected: true, Reason: err.Error()}, nil
	}

	target, err := resolveOrderStoreTarget(m.cityPath, m.cfg, a)
	if err != nil {
		return orderdispatch.DispatchResult{ScopedName: scoped}, fmt.Errorf("resolving store target for %s: %w", scoped, err)
	}
	store, err := m.storeFn(target)
	if err != nil {
		return orderdispatch.DispatchResult{ScopedName: scoped}, fmt.Errorf("opening store for %s: %w", scoped, err)
	}

	// Durable correlation is checked and created under one controller-local
	// state. The native tracking bead is the durable authority; the in-memory
	// active set only prevents a same-process replay from relaunching a dispatch
	// whose tracking bead exists but whose formula graph is not materialized yet.
	deliveryID := strings.TrimSpace(req.ExternalDeliveryID)
	if deliveryID != "" {
		state := m.webhookDeliveryStateForUse()
		state.mu.Lock()
		localKey := scoped + "\x00" + deliveryID
		if trackingID := state.active[localKey]; trackingID != "" {
			state.mu.Unlock()
			closeBeadStoreHandle(store)
			return orderdispatch.DispatchResult{
				ScopedName: scoped,
				TrackingID: trackingID,
				Fired:      true,
				Reused:     true,
			}, nil
		}
		frontDoor := m.orderFrontDoorFor(store)
		existing, found, findErr := frontDoor.FindRunByExternalDelivery(scoped, deliveryID)
		if findErr != nil {
			state.mu.Unlock()
			closeBeadStoreHandle(store)
			return orderdispatch.DispatchResult{ScopedName: scoped}, fmt.Errorf("checking webhook delivery %q for %s: %w", deliveryID, scoped, findErr)
		}
		if found {
			// Exec orders have no graph materialization to recover; the native
			// tracking run itself is the durable completed/replayed result.
			if a.IsExec() {
				state.mu.Unlock()
				closeBeadStoreHandle(store)
				return orderdispatch.DispatchResult{ScopedName: scoped, TrackingID: existing.ID, Fired: true, Reused: true}, nil
			}

			materializationKey := orders.ExternalDeliveryIdempotencyKey(scoped, deliveryID)
			_, materialized, materializeErr := findExternalDeliveryMaterialization(m.graphStoreFor(store), materializationKey)
			if materializeErr != nil {
				state.mu.Unlock()
				closeBeadStoreHandle(store)
				return orderdispatch.DispatchResult{ScopedName: scoped}, fmt.Errorf("checking webhook materialization %q for %s: %w", deliveryID, scoped, materializeErr)
			}
			if materialized {
				state.mu.Unlock()
				closeBeadStoreHandle(store)
				return orderdispatch.DispatchResult{ScopedName: scoped, TrackingID: existing.ID, Fired: true, Reused: true}, nil
			}
			if !existing.Open {
				state.mu.Unlock()
				closeBeadStoreHandle(store)
				return orderdispatch.DispatchResult{ScopedName: scoped}, fmt.Errorf("webhook delivery %q for %s has a closed tracking run %s but no materialized workflow", deliveryID, scoped, existing.ID)
			}

			state.active[localKey] = existing.ID
			state.mu.Unlock()
			closeStore := m.webhookDispatchCloser(store, localKey, scoped)
			m.launchExistingDispatch(ctx, store, target, a, m.cityPath, existing.ID, materializationKey, req.Vars, req.ExecEnv, closeStore)
			return orderdispatch.DispatchResult{ScopedName: scoped, TrackingID: existing.ID, Fired: true, Reused: true}, nil
		}
		trackingRun, createErr := m.orderFrontDoorFor(store).CreateRun(scoped, orders.RunOpts{ExternalDeliveryID: deliveryID})
		if createErr != nil {
			state.mu.Unlock()
			closeBeadStoreHandle(store)
			return orderdispatch.DispatchResult{ScopedName: scoped}, fmt.Errorf("creating tracking bead for %s: %w", scoped, createErr)
		}
		state.active[localKey] = trackingRun.ID
		state.mu.Unlock()

		materializationKey := orders.ExternalDeliveryIdempotencyKey(scoped, deliveryID)
		closeStore := m.webhookDispatchCloser(store, localKey, scoped)
		if m.testAfterTrackingCreated != nil && m.testAfterTrackingCreated(trackingRun) {
			// The test seam intentionally leaves the durable tracking bead open
			// and closes only this request's store handle: the next dispatcher is
			// the process that recovers the incomplete native run.
			closeStore()
			return orderdispatch.DispatchResult{ScopedName: scoped, TrackingID: trackingRun.ID, Fired: true}, nil
		}
		m.addInflight()
		m.launchDispatchOne(ctx, store, target, a, m.cityPath, trackingRun.ID, materializationKey, req.Vars, req.ExecEnv, closeStore)
		return orderdispatch.DispatchResult{ScopedName: scoped, TrackingID: trackingRun.ID, Fired: true}, nil
	}

	// Close this dispatch's own store handle once the async dispatchOne goroutine
	// has finished with it. Mirrors the tick loop's detached per-tick closer: the
	// handle must stay open until the goroutine's final store call (the
	// tracking-bead close), so the close is deferred to onDone — launchDispatchOne
	// invokes it only after dispatchOne returns (gascity#3157). The two exit paths
	// are mutually exclusive (a create failure never launches), so the closer runs
	// exactly once.
	closeStore := func() {
		if cerr := closeBeadStoreHandle(store); cerr != nil {
			logDispatchError(m.stderr, "gc: webhook dispatch: closing store for %s: %v", scoped, cerr)
		}
	}

	trackingRun, err := m.launchResolvedDispatchWithOpts(ctx, store, target, a, m.cityPath, req.Vars, req.ExecEnv, closeStore, orders.RunOpts{
		ExternalDeliveryID: deliveryID,
	})
	if err != nil {
		closeStore() // nothing launched; release the handle we opened
		return orderdispatch.DispatchResult{ScopedName: scoped}, fmt.Errorf("creating tracking bead for %s: %w", scoped, err)
	}
	return orderdispatch.DispatchResult{ScopedName: scoped, TrackingID: trackingRun.ID, Fired: true}, nil
}

func findExternalDeliveryMaterialization(store beads.Store, key string) (string, bool, error) {
	if store == nil || strings.TrimSpace(key) == "" {
		return "", false, nil
	}
	rows, err := store.ListByMetadata(
		// molecule.Instantiate's existing root option uses the generic
		// idempotency_key metadata field (the Attach path additionally uses
		// gc.idempotency_key). Keep this recovery lookup on the Instantiate
		// contract so it observes both graph-apply and sequential roots.
		map[string]string{"idempotency_key": key},
		2,
		beads.IncludeClosed,
		beads.WithBothTiers,
	)
	if err != nil {
		return "", false, err
	}
	if len(rows) > 1 {
		return "", false, fmt.Errorf("materialization key %q has %d workflow roots", key, len(rows))
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	return rows[0].ID, true, nil
}

func (m *memoryOrderDispatcher) webhookDispatchCloser(store beads.Store, localKey, scoped string) func() {
	return func() {
		m.releaseWebhookDeliveryKey(localKey)
		if cerr := closeBeadStoreHandle(store); cerr != nil {
			logDispatchError(m.stderr, "gc: webhook dispatch: closing store for %s: %v", scoped, cerr)
		}
	}
}

func (m *memoryOrderDispatcher) releaseWebhookDeliveryKey(localKey string) {
	state := m.webhookDeliveryStateForUse()
	state.mu.Lock()
	delete(state.active, localKey)
	state.mu.Unlock()
}
