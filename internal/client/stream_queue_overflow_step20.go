// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package client

import (
	"context"

	"masterdnsvpn-go/internal/config"
	"masterdnsvpn-go/internal/metrics"
)

// Step 20 — Backpressure & Bounded Queues
//
// This file holds the policy-aware enqueue helpers for the two client-
// side bounded channels that historically blocked the producer
// indefinitely under sustained burst:
//
//   - plannerQueue   (chan plannerTask)
//   - encodedTXChannel (chan writerTask)
//
// Both channels are sized by ClientConfig.RX_TX_Workers; under
// well-tuned operating conditions they never fill. Under abuse (huge
// inbound burst, slow link, GC pause on the writer goroutine) the
// producer side previously parked, holding a reference to the task and
// its underlying TX packet — that turned channel saturation into
// unbounded heap retention.
//
// The new knob `STREAM_QUEUE_OVERFLOW_POLICY` lets the operator choose
// from three behaviours:
//
//   - "block" / unset (default): exactly the pre-Step-20 behaviour.
//   - "drop-newest":              if the channel is full, drop the
//                                 incoming task. The producer never
//                                 parks.
//   - "drop-oldest":              if the channel is full, evict the
//                                 oldest queued task (releasing its
//                                 packet) and enqueue the new one. The
//                                 producer never parks.
//
// Dropped tasks have any non-packed TX packet released back to the
// stream's pool so allocation accounting stays consistent. Metrics
// counters StreamQueueDropsNewest / StreamQueueDropsOldest are bumped
// per drop so the operator can correlate memory pressure with policy
// activity. Both counters stay at zero on the default "block" path.

// releasePlannerTask returns any TX packet borrowed by a planner task
// back to its owning stream. Packed tasks (wasPacked == true) never own
// a per-stream packet, so this helper is a no-op for them. Safe to
// call on a zero-value task.
func releasePlannerTask(task plannerTask) {
	if task.wasPacked {
		return
	}
	if task.selected == nil || task.item == nil {
		return
	}
	task.selected.ReleaseTXPacket(task.item)
}

// releaseWriterTask is the writerTask counterpart of releasePlannerTask.
// Identical shape because writerTask carries the same (wasPacked, item,
// selected) triplet that planner does.
func releaseWriterTask(task writerTask) {
	if task.wasPacked {
		return
	}
	if task.selected == nil || task.item == nil {
		return
	}
	task.selected.ReleaseTXPacket(task.item)
}

// dispatchPlannerTask enqueues a planner task subject to the configured
// overflow policy. Returns true when the task was accepted (either
// directly or after an oldest-eviction), false when the task was
// dropped or the context was cancelled before any slot opened.
//
// Hot path note: the policy is resolved once via
// ClientConfig.EffectiveStreamQueueOverflowMode (parsed integer enum)
// so the per-packet cost is a single integer switch — no string
// comparisons on the hot path.
func (c *Client) dispatchPlannerTask(ctx context.Context, task plannerTask) bool {
	switch c.streamQueueOverflowMode() {
	case config.StreamQueueOverflowDropNewest:
		select {
		case c.plannerQueue <- task:
			return true
		default:
			metrics.StreamQueueDropsNewest.Add(1)
			releasePlannerTask(task)
			return false
		}
	case config.StreamQueueOverflowDropOldest:
		// Try the non-blocking enqueue first — the common case is that
		// the queue has room and we hit the cheap path. Only if full
		// do we evict.
		select {
		case c.plannerQueue <- task:
			return true
		default:
		}
		// Pop one oldest task to make room. If a concurrent consumer
		// already drained a slot we'll grab nothing — that's fine, the
		// next send below will succeed without an eviction.
		select {
		case evicted := <-c.plannerQueue:
			metrics.StreamQueueDropsOldest.Add(1)
			releasePlannerTask(evicted)
		default:
		}
		// Now the second send. ctx.Done is honored so we don't park
		// against an empty channel during shutdown.
		select {
		case c.plannerQueue <- task:
			return true
		case <-ctx.Done():
			releasePlannerTask(task)
			return false
		}
	default: // StreamQueueOverflowBlock — preserve pre-Step-20 semantics
		select {
		case c.plannerQueue <- task:
			return true
		case <-ctx.Done():
			releasePlannerTask(task)
			return false
		}
	}
}

// dispatchWriterTask is the encoded-tx-channel counterpart of
// dispatchPlannerTask.
func (c *Client) dispatchWriterTask(ctx context.Context, task writerTask) bool {
	switch c.streamQueueOverflowMode() {
	case config.StreamQueueOverflowDropNewest:
		select {
		case c.encodedTXChannel <- task:
			return true
		default:
			metrics.StreamQueueDropsNewest.Add(1)
			releaseWriterTask(task)
			return false
		}
	case config.StreamQueueOverflowDropOldest:
		select {
		case c.encodedTXChannel <- task:
			return true
		default:
		}
		select {
		case evicted := <-c.encodedTXChannel:
			metrics.StreamQueueDropsOldest.Add(1)
			releaseWriterTask(evicted)
		default:
		}
		select {
		case c.encodedTXChannel <- task:
			return true
		case <-ctx.Done():
			releaseWriterTask(task)
			return false
		}
	default: // StreamQueueOverflowBlock
		select {
		case c.encodedTXChannel <- task:
			return true
		case <-ctx.Done():
			releaseWriterTask(task)
			return false
		}
	}
}

// streamQueueOverflowMode returns the resolved overflow mode for this
// client. It is safe on a nil client (returns Block) so wiring callers
// don't have to nil-check.
func (c *Client) streamQueueOverflowMode() config.StreamQueueOverflowMode {
	if c == nil {
		return config.StreamQueueOverflowBlock
	}
	return c.cfg.EffectiveStreamQueueOverflowMode()
}
