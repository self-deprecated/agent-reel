package reel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

const producerSlotsMax = 128
const producerChangesMax = 1024
const producerDescriptorMax = 2048
const producerLeaseAge = 5 * time.Minute

type producerDescriptor struct {
	Producer      string            `json:"producer"`
	AgentInstance *string           `json:"agentInstance"`
	Conversation  *string           `json:"conversation"`
	Parent        *string           `json:"parent"`
	Location      *activityLocation `json:"location"`
}
type producerContext struct {
	Generation string             `json:"generation"`
	Descriptor producerDescriptor `json:"descriptor"`
}
type producerLease struct {
	Generation string             `json:"generation"`
	Descriptor producerDescriptor `json:"descriptor"`
	ExpiresAt  time.Time          `json:"expiresAt"`
	Durability string             `json:"durability"`
}
type producerCounts struct {
	Slots    int    `json:"slots"`
	Changes  int    `json:"changes"`
	Rejected uint64 `json:"rejected"`
}
type producerSlot struct {
	descriptor           producerDescriptor
	generation, previous string
	expires              time.Time // receiver monotonic component retained; never use producer time
}
type producerInput struct {
	Descriptor producerDescriptor `json:"descriptor"`
	Generation string             `json:"generation,omitempty"`
}

func descriptorOf(v activityEnvelope) producerDescriptor {
	d := producerDescriptor{AgentInstance: v.AgentInstance, Conversation: v.Conversation, Parent: v.Parent, Location: v.Location}
	if v.Producer != nil {
		d.Producer = *v.Producer
	}
	return d
}
func validateProducer(d producerDescriptor, target Target) error {
	data, e := json.Marshal(d)
	if e != nil || len(data) > producerDescriptorMax {
		return fmt.Errorf("producer descriptor exceeds bound")
	}
	v := activityEnvelope{ID: "descriptor", Producer: &d.Producer, AgentInstance: d.AgentInstance, Conversation: d.Conversation, Parent: d.Parent, Location: d.Location, Source: "areel:descriptor", Kind: "areel:descriptor"}
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = parseActivity(raw, target)
	return e
}
func sameProducer(a, b producerDescriptor) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func copyProducer(d producerDescriptor) producerDescriptor {
	b, _ := json.Marshal(d)
	var clone producerDescriptor
	_ = json.Unmarshal(b, &clone)
	return clone
}

// All registration operations serialize with event admission under the existing
// short in-memory mutex. No filesystem work, per-producer goroutine or eviction.
func (s *activityService) producerOperation(operation string, d producerDescriptor, expected string) activityResponse {
	if e := validateProducer(d, s.state.Target); e != nil {
		return s.producerReject(e.Error())
	}
	if operation == "producer-register" && expected != "" {
		return s.producerReject("register cannot name an expected generation")
	}
	if operation != "producer-register" && !validID.MatchString(expected) {
		return s.producerReject("expected producer generation required")
	}
	// Entropy acquisition is outside the admission mutex, including retries that
	// eventually discard this candidate. Only issued generations consume budget.
	nextGeneration := ""
	if operation == "producer-register" || operation == "producer-replace" {
		nextGeneration = newID()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reject := func(message string) activityResponse {
		increment(&s.accounting.Rejected)
		increment(&s.producerStats.Rejected)
		return s.producerReplyLocked(nil, message)
	}
	if s.closing {
		return reject("holder closing")
	}
	now := s.now()
	slot := s.producers[d.Producer]
	switch operation {
	case "producer-register":
		if slot != nil {
			if !sameProducer(slot.descriptor, d) {
				return reject("producer already registered with a different descriptor")
			}
			if !now.Before(slot.expires) {
				return reject("producer lease expired; explicit replacement required")
			}
			return s.producerReplyLocked(slot, "") // idempotent register, no implicit refresh
		}
		if len(s.producers) >= producerSlotsMax {
			return reject("producer slot limit reached")
		}
		if s.producerStats.Changes >= producerChangesMax {
			return reject("producer generation change limit reached")
		}
		slot = &producerSlot{descriptor: copyProducer(d), generation: nextGeneration, expires: now.Add(producerLeaseAge)}
		s.producers[d.Producer] = slot
		s.producerStats.Slots++
		s.producerStats.Changes++
	case "producer-refresh":
		if slot == nil || slot.generation != expected {
			return reject("stale or unknown producer generation")
		}
		if !sameProducer(slot.descriptor, d) {
			return reject("producer refresh descriptor mismatch")
		}
		if !now.Before(slot.expires) {
			return reject("producer lease expired; explicit replacement required")
		}
		slot.expires = now.Add(producerLeaseAge)
	case "producer-replace":
		if slot == nil {
			return reject("unknown producer")
		}
		if slot.generation != expected {
			// One bounded retry identity per slot: a lost last replacement response
			// can be recovered, but cannot refresh or apply the replacement twice.
			if slot.previous == expected && sameProducer(slot.descriptor, d) && now.Before(slot.expires) {
				return s.producerReplyLocked(slot, "")
			}
			return reject("stale producer replacement")
		}
		if s.producerStats.Changes >= producerChangesMax {
			return reject("producer generation change limit reached")
		}
		slot.previous = slot.generation
		slot.generation = nextGeneration
		slot.descriptor = copyProducer(d)
		slot.expires = now.Add(producerLeaseAge)
		s.producerStats.Changes++
	default:
		return reject("unknown producer operation")
	}
	return s.producerReplyLocked(slot, "")
}
func (s *activityService) producerReplyLocked(slot *producerSlot, message string) activityResponse {
	counts := s.producerStats
	res := activityResponse{Accounting: s.accounting, Registry: &counts, Error: message}
	if slot != nil {
		res.Registration = &producerLease{Generation: slot.generation, Descriptor: copyProducer(slot.descriptor), ExpiresAt: slot.expires.UTC(), Durability: "volatile-holder-lifetime"}
	}
	return res
}
func (s *activityService) producerReject(message string) activityResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	increment(&s.producerStats.Rejected)
	increment(&s.accounting.Rejected)
	return s.producerReplyLocked(nil, message)
}

// Called under the admission mutex only for a NEW registered event. Exact old
// retries are handled first by the retained context-aware receipt, not rebound.
func (s *activityService) checkProducerLocked(context *producerContext, now time.Time) error {
	if context == nil {
		return nil
	}
	slot := s.producers[context.Descriptor.Producer]
	if slot == nil || slot.generation != context.Generation {
		return fmt.Errorf("stale or unknown producer generation")
	}
	if !now.Before(slot.expires) {
		return fmt.Errorf("producer lease expired")
	}
	if !sameProducer(slot.descriptor, context.Descriptor) {
		return fmt.Errorf("registered event descriptor mismatch")
	}
	return nil
}
