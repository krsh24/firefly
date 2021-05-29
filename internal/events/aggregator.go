// Copyright © 2021 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package events

import (
	"context"

	"github.com/kaleido-io/firefly/internal/broadcast"
	"github.com/kaleido-io/firefly/internal/config"
	"github.com/kaleido-io/firefly/internal/data"
	"github.com/kaleido-io/firefly/internal/log"
	"github.com/kaleido-io/firefly/internal/retry"
	"github.com/kaleido-io/firefly/pkg/database"
	"github.com/kaleido-io/firefly/pkg/fftypes"
)

const (
	aggregatorOffsetName = "ff-aggregator"
)

type eventsByRef map[fftypes.UUID][]*fftypes.Event

func (ebr eventsByRef) hasAnyOf(ref fftypes.UUID, types ...fftypes.EventType) bool {
	events, ok := ebr[ref]
	if ok {
		for _, ev := range events {
			for _, t := range types {
				if ev.Type == t {
					return true
				}
			}
		}
	}
	return false
}

func (ebr eventsByRef) remove(eventID fftypes.UUID) bool {
	for ref, events := range ebr {
		for i, ev := range events {
			if eventID.Equals(ev.ID) {
				ebr[ref] = append(ebr[ref][0:i], ebr[ref][i+1:]...)
				return true
			}
		}
	}
	return false
}

type aggregator struct {
	ctx         context.Context
	database    database.Plugin
	broadcast   broadcast.Manager
	data        data.Manager
	eventPoller *eventPoller
}

func newAggregator(ctx context.Context, di database.Plugin, bm broadcast.Manager, dm data.Manager, en *eventNotifier) *aggregator {
	ag := &aggregator{
		ctx:       log.WithLogField(ctx, "role", "aggregator"),
		database:  di,
		broadcast: bm,
		data:      dm,
	}
	firstEvent := fftypes.SubOptsFirstEvent(config.GetString(config.EventAggregatorFirstEvent))
	ag.eventPoller = newEventPoller(ctx, di, en, &eventPollerConf{
		eventBatchSize:             config.GetInt(config.EventAggregatorBatchSize),
		eventBatchTimeout:          config.GetDuration(config.EventAggregatorBatchTimeout),
		eventPollTimeout:           config.GetDuration(config.EventAggregatorPollTimeout),
		startupOffsetRetryAttempts: config.GetInt(config.OrchestratorStartupAttempts),
		retry: retry.Retry{
			InitialDelay: config.GetDuration(config.EventAggregatorRetryInitDelay),
			MaximumDelay: config.GetDuration(config.EventAggregatorRetryMaxDelay),
			Factor:       config.GetFloat64(config.EventAggregatorRetryFactor),
		},
		firstEvent:       &firstEvent,
		offsetType:       fftypes.OffsetTypeAggregator,
		offsetNamespace:  fftypes.SystemNamespace,
		offsetName:       aggregatorOffsetName,
		newEventsHandler: ag.processEventRetryAndGroup,
	})
	return ag
}

func (ag *aggregator) start() error {
	return ag.eventPoller.start()
}

func (ag *aggregator) processEventRetryAndGroup(events []*fftypes.Event) (repoll bool, err error) {
	err = ag.database.RunAsGroup(ag.ctx, func(ctx context.Context) (err error) {
		repoll, err = ag.processEvents(ctx, events)
		return err
	})
	return repoll, err
}

func (ag *aggregator) processEvents(ctx context.Context, events []*fftypes.Event) (repoll bool, err error) {
	// Build a map by reference, to allow optimizing out double-processing
	lookahead := make(eventsByRef)
	for _, event := range events {
		lookahead[*event.Reference] = append(lookahead[*event.Reference], event)
	}

	for _, event := range events {
		removed := lookahead.remove(*event.ID)
		log.L(ctx).Debugf("Aggregating event %.10d/%s [%s]: %s/%s (%t)", event.Sequence, event.ID, event.Type, event.Namespace, event.Reference, removed)

		repoll, err = ag.processEvent(ctx, lookahead, event)
		if err != nil {
			return false, err
		}
	}
	err = ag.eventPoller.commitOffset(ctx, events[len(events)-1].Sequence)
	return repoll, err
}

func (ag *aggregator) processEvent(ctx context.Context, batchRefs eventsByRef, event *fftypes.Event) (bool, error) {
	l := log.L(ctx)
	switch event.Type {
	case fftypes.EventTypeDataArrivedBroadcast:
		return ag.processDataArrived(ctx, event.Namespace, batchRefs, event)
	case fftypes.EventTypeMessageSequencedBroadcast:
		msg, err := ag.database.GetMessageByID(ctx, event.Reference)
		if err != nil {
			return false, err
		}
		if msg != nil && msg.Confirmed == nil {
			return ag.checkMessageComplete(ctx, msg, batchRefs, event)
		}
	default:
		// Other events do not need aggregation.
		// Note this MUST include all events that are generated via aggregation, or we would infinite loop
	}
	l.Debugf("No action for event %.10d/%s [%s]: %s/%s", event.Sequence, event.ID, event.Type, event.Namespace, event.Reference)
	return false, nil
}

func (ag *aggregator) processDataArrived(ctx context.Context, ns string, lookahead eventsByRef, event *fftypes.Event) (bool, error) {
	l := log.L(ctx)

	// Find all unconfirmed messages associated with this data
	fb := database.MessageQueryFactory.NewFilter(ctx)
	msgs, err := ag.database.GetMessagesForData(ctx, event.Reference, fb.And(
		fb.Eq("namespace", ns),
		fb.Eq("confirmed", nil),
	).Sort("sequence"))
	if err != nil {
		return false, err
	}
	repoll := false
	for _, msg := range msgs {
		l.Infof("Data %s arrived for message %s", event.Reference, msg.Header.ID)
		if lookahead.hasAnyOf(*msg.Header.ID, fftypes.EventTypeMessageSequencedBroadcast) {
			l.Debugf("Not triggering completion check on message, due to lookahead detection of upcoming event")
			continue
		}
		msgRepoll, err := ag.checkMessageComplete(ctx, msg, lookahead, event)
		if err != nil {
			return false, err
		}
		repoll = repoll || msgRepoll
	}
	return repoll, nil
}

func (ag *aggregator) checkUpdateContextBlocked(ctx context.Context, msg *fftypes.Message, complete bool) (blocked *fftypes.Blocked, err error) {
	l := log.L(ctx)

	// Need to check if the context is already blocked
	changed := false
	blocked, err = ag.database.GetBlockedByContext(ctx, msg.Header.Namespace, msg.Header.Context, msg.Header.Group)
	if err != nil {
		return nil, err
	}
	if blocked == nil && !complete {
		changed = true
		blocked = &fftypes.Blocked{
			ID:        fftypes.NewUUID(),
			Namespace: msg.Header.Namespace,
			Context:   msg.Header.Context,
			Group:     msg.Header.Group,
			Created:   fftypes.Now(),
			Message:   msg.Header.ID,
		}
		if err = ag.database.UpsertBlocked(ctx, blocked, false); err != nil {
			return nil, err
		}
	}
	if blocked != nil {
		l.Infof("Context %s:%s [group=%v] BLOCKED by %s changed=%t", msg.Header.Namespace, msg.Header.Context, msg.Header.Group, blocked.Message, changed)
	}
	return blocked, nil
}

func (ag *aggregator) checkMessageComplete(ctx context.Context, msg *fftypes.Message, lookahead eventsByRef, event *fftypes.Event) (bool, error) {
	l := log.L(ctx)

	// If we're triggering because of data arriving, we need to make sure the message itself has arrived
	if !event.Reference.Equals(msg.Header.ID) {
		fb := database.EventQueryFactory.NewFilter(ctx)
		filter := fb.And(
			fb.Eq("reference", msg.Header.ID),
			fb.Eq("type", fftypes.EventTypeMessageSequencedBroadcast),
		).Limit(1)
		msgs, err := ag.database.GetEvents(ctx, filter)
		if err != nil {
			return false, err
		}
		if len(msgs) < 1 {
			return false, nil
		}
	}

	// Check this message is complete
	complete, err := ag.database.CheckDataAvailable(ctx, msg)
	if err != nil {
		return false, err // CheckDataAvailable should only return an error if there's a problem with persistence
	}

	// Check if the context is currently blocked, or we will block it.
	blockedBy, err := ag.checkUpdateContextBlocked(ctx, msg, complete)
	if err != nil {
		return false, err
	}
	if !complete || (blockedBy != nil && !blockedBy.Message.Equals(msg.Header.ID)) {
		return false, err
	}

	repoll := false

	// Process system messgaes
	if msg.Header.Namespace == fftypes.SystemNamespace {
		// We handle system events in-line on the aggregator, as it would be confusing for apps to be
		// dispatched subsequent events before we have processed the system events they depend on.
		if err = ag.broadcast.HandleSystemBroadcast(ctx, msg); err != nil {
			// Should only return errors that are retryable
			return false, err
		}
	}

	// This message is now confirmed
	setConfirmed := database.MessageQueryFactory.NewUpdate(ctx).Set("confirmed", fftypes.Now())
	err = ag.database.UpdateMessage(ctx, msg.Header.ID, setConfirmed)
	if err != nil {
		return false, err
	}

	// Emit the confirmed event
	confirmedEvent := fftypes.NewEvent(fftypes.EventTypeMessageConfirmed, msg.Header.Namespace, msg.Header.ID)
	if err = ag.database.UpsertEvent(ctx, confirmedEvent, false); err != nil {
		return false, err
	}

	unblock := blockedBy != nil && blockedBy.Message.Equals(msg.Header.ID)
	l.Infof("Context %s:%s [group=%d] - message %s confirmed (unblock=%t)", msg.Header.Namespace, msg.Header.Context, msg.Header.Group, msg.Header.ID, unblock)

	// Are we unblocking a context?
	if unblock {
		// Check forwards to see if there is a message we could unblock
		fb := database.MessageQueryFactory.NewFilter(ctx)
		filter := fb.And(
			fb.Eq("namespace", msg.Header.Namespace),
			fb.Eq("group", msg.Header.Group),
			fb.Eq("context", msg.Header.Context),
			fb.Gt("sequence", msg.Sequence), // Above our sequence
			fb.Eq("confirmed", nil),
		).Sort("sequence").Limit(1) // only need one message
		unblockable, err := ag.database.GetMessageRefs(ctx, filter)
		if err != nil {
			return false, err
		}
		if len(unblockable) > 0 {
			unblockMsg := unblockable[0]

			// Update the blocked record to this new message
			l.Infof("Message %s (seq=%d) unblocks message %s (seq=%d)", msg.Header.ID, msg.Sequence, unblockMsg.ID, unblockMsg.Sequence)
			u := database.BlockedQueryFactory.NewUpdate(ctx).Set("message", unblockMsg.ID)
			if err = ag.database.UpdateBlocked(ctx, blockedBy.ID, u); err != nil {
				return false, err
			}

			// We can optimize out the extra event, if we know there's an event that will trigger
			// the same processing in the lookahead buffer
			if lookahead.hasAnyOf(*unblockMsg.ID, fftypes.EventTypeMessageConfirmed, fftypes.EventTypeMessageSequencedBroadcast) {
				l.Debugf("Not queuing unblocked event for %s, due to lookahead detection of upcoming event", unblockMsg.ID)
			} else {
				// Emit an event to cause the aggregator to process that message
				unblockedEvent := fftypes.NewEvent(fftypes.EventTypeMessagesUnblocked, msg.Header.Namespace, unblockable[0].ID)
				if err = ag.database.UpsertEvent(ctx, unblockedEvent, false); err != nil {
					return false, err
				}
				// We want the loop to fire again, to pick up this unblock event
				repoll = true

			}
		} else if err = ag.database.DeleteBlocked(ctx, blockedBy.ID); err != nil {
			return false, err
		}
	}
	return repoll, nil
}
