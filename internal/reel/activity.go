package reel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	activityEnvelopeMax    = 8 << 10
	activityRequestMax     = 16 << 10
	activityRecordMax      = 16 << 10
	activityConnectionsMax = 8
	activityWait           = 500 * time.Millisecond
)

// footageAnchor is the fixed shape of an activity record's footage anchor.
// Indexed rows carry placements instead, so a reconstructed record's anchor
// is always null.
type footageAnchor struct {
	Segment   int     `json:"segment"`
	Seconds   float64 `json:"seconds"`
	Lines     int64   `json:"lines"`
	Basis     string  `json:"basis"`
	Precision string  `json:"precision"`
}

var activityName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}:[A-Za-z0-9._/-]{1,127}$`)

type activityLocation struct {
	Backend  string  `json:"backend"`
	Instance string  `json:"instance"`
	Pane     *string `json:"pane"`
	Tab      *string `json:"tab"`
}

// Attribution and intent are declarations, never authentication or visibility evidence.
type activityEnvelope struct {
	ID                string            `json:"id"`
	Producer          *string           `json:"producer"`
	AgentInstance     *string           `json:"agentInstance"`
	Conversation      *string           `json:"conversation"`
	Parent            *string           `json:"parent"`
	Location          *activityLocation `json:"location"`
	Source            string            `json:"source"`
	Kind              string            `json:"kind"`
	Reason            *string           `json:"reason"`
	Initiator         *string           `json:"initiator"`
	ProducerTimestamp *string           `json:"producerTimestamp"`
	Label             *string           `json:"label"`
	Data              json.RawMessage   `json:"data"`
}
type activityReceipt struct {
	ID       string    `json:"id"`
	Sequence uint64    `json:"sequence"`
	At       time.Time `json:"at"`
}

// activityRecord is the record shape readers show for an indexed row: every
// outer field is always emitted. Registration is null for declaration-only
// events and the retained producer context for registered ones.
type activityRecord struct {
	Recording     string           `json:"recording"`
	Backend       string           `json:"backend"`
	Instance      string           `json:"instance"`
	Receipt       activityReceipt  `json:"receipt"`
	Transport     string           `json:"observedTransport"`
	Attribution   string           `json:"attribution"`
	Visibility    string           `json:"visibility"`
	FootageAnchor *footageAnchor   `json:"footageAnchor"`
	Marker        bool             `json:"marker"`
	Envelope      activityEnvelope `json:"envelope"`
	Registration  *producerContext `json:"registration"`
}

// activityRequest is the one private wire shape. Registration is expressed by
// the operation plus descriptor/generation, not by a protocol version.
type activityRequest struct {
	Token      string              `json:"token"`
	Recording  string              `json:"recording"`
	Backend    string              `json:"backend"`
	Instance   string              `json:"instance"`
	Operation  string              `json:"operation"`
	Envelope   json.RawMessage     `json:"envelope,omitempty"`
	Descriptor *producerDescriptor `json:"descriptor,omitempty"`
	Generation string              `json:"generation,omitempty"`
}
type activityAccounting struct {
	Requests      uint64 `json:"requests"`
	Accepted      uint64 `json:"accepted"`
	Durable       uint64 `json:"durable"`
	Rejected      uint64 `json:"rejected"`
	Duplicates    uint64 `json:"duplicates"`
	Failed        uint64 `json:"failed"`
	Pending       uint64 `json:"pending"`
	ReservedBytes uint64 `json:"reservedBytes"`
	DurableBytes  uint64 `json:"durableBytes"`
}
type activityResponse struct {
	Accepted          bool               `json:"accepted"`
	Durable           bool               `json:"durable"`
	Failed            bool               `json:"failed"`
	Duplicate         bool               `json:"duplicate"`
	Receipt           *activityReceipt   `json:"receipt"`
	Accounting        activityAccounting `json:"accounting"`
	Error             string             `json:"error,omitempty"`
	Registration      *producerLease     `json:"registration,omitempty"`
	RegisteredContext *producerContext   `json:"registeredContext,omitempty"`
	Registry          *producerCounts    `json:"registry,omitempty"`
}
type activityEntry struct {
	registration    *producerContext
	receipt         activityReceipt
	hash            string
	done            chan struct{}
	durable, failed bool
	err             string
}
type activityService struct {
	store            *Store
	state            State // immutable binding only; no shared machine state
	spec             launchSpec
	dir              string
	listener         net.Listener
	hook             func(string) error
	mu               sync.Mutex // never held across I/O, hooks or waits
	accounting       activityAccounting
	sinkErr          string
	closing          bool
	closed, finished chan struct{}
	closeOnce        sync.Once
	connections      chan struct{}
	now              func() time.Time
	producers        map[string]*producerSlot
	producerStats    producerCounts
	// sink is the one activity sink of this holder: indexSink, the indexed
	// activity store. It shares this service's admission mutex, accounting,
	// registry and reply shape.
	sink activitySink
	// placement is the holder's placement clock (see placementSource); the
	// sink reads it under the admission mutex, without I/O.
	placement placementSource
}

// activitySink is the internal seam between admission and storage. admit runs
// after the request is parsed and any registration context is built; it owns
// dedup, sequence assignment and queueing. run is the single writer goroutine
// and must close s.finished when it returns.
type activitySink interface {
	admit(v activityEnvelope, marker bool, registration *producerContext) activityResponse
	run()
}

func (s *activityService) setPlacement(source placementSource) {
	s.mu.Lock()
	s.placement = source
	s.mu.Unlock()
}

// strictActivityJSON rejects duplicate keys as well as unknown typed fields.
// Limits on bytes and depth bound both the token walk and the typed decode.
func strictActivityJSON(data []byte, out any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return fmt.Errorf("JSON depth exceeds 32")
		}
		tok, e := dec.Token()
		if e != nil {
			return e
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			keys := map[string]bool{}
			for dec.More() {
				k, e := dec.Token()
				if e != nil {
					return e
				}
				key, ok := k.(string)
				if !ok || keys[key] {
					return fmt.Errorf("duplicate or invalid JSON key")
				}
				keys[key] = true
				if e = value(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for dec.More() {
				if e = value(depth + 1); e != nil {
					return e
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, e = dec.Token()
		return e
	}
	if e := value(0); e != nil {
		return e
	}
	if _, e := dec.Token(); e != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	if e := activitySchemaKeys(data, reflect.TypeOf(out)); e != nil {
		return e
	}
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

// encoding/json alone also accepts case-insensitive aliases for typed fields.
// Reject those aliases; open extension RawMessage keys are not restricted.
func activitySchemaKeys(data []byte, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || typ == reflect.TypeOf(time.Time{}) {
		return nil
	}
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(data, &fields); e != nil {
		return e
	}
	for key, value := range fields {
		found := false
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if key == strings.Split(field.Tag.Get("json"), ",")[0] {
				found = true
				if e := activitySchemaKeys(value, field.Type); e != nil {
					return e
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown or noncanonical JSON field")
		}
	}
	return nil
}
func activityText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}
func parseActivity(raw []byte, target Target) (activityEnvelope, error) {
	var v activityEnvelope
	if len(raw) > activityEnvelopeMax {
		return v, fmt.Errorf("activity envelope exceeds 8192 bytes")
	}
	if e := strictActivityJSON(raw, &v); e != nil {
		return v, fmt.Errorf("invalid activity JSON")
	}
	return v, validateActivityEnvelope(v, target)
}

// validateActivityEnvelope checks fields after strict decoding. The raw ingress
// byte cap is not a bound on a writer-canonicalized stored envelope.
func validateActivityEnvelope(v activityEnvelope, target Target) error {
	if !activityText(v.ID, 128) || !activityName.MatchString(v.Source) || !activityName.MatchString(v.Kind) {
		return fmt.Errorf("invalid activity ID or namespaced source/kind")
	}
	for _, p := range []*string{v.Producer, v.AgentInstance, v.Conversation, v.Parent, v.Initiator, v.ProducerTimestamp} {
		if p != nil && !activityText(*p, 128) {
			return fmt.Errorf("invalid bounded attribution/intent/timestamp")
		}
	}
	if v.Label != nil && !activityText(*v.Label, 256) {
		return fmt.Errorf("invalid activity label")
	}
	if v.Reason != nil && !activityName.MatchString(*v.Reason) {
		return fmt.Errorf("invalid namespaced reason")
	}
	if v.Location != nil {
		if v.Location.Backend != target.Backend || v.Location.Instance != target.Instance {
			return fmt.Errorf("activity location conflicts with recording binding")
		}
		for _, p := range []*string{v.Location.Pane, v.Location.Tab} {
			if p != nil && !activityText(*p, 128) {
				return fmt.Errorf("invalid opaque pane/tab")
			}
		}
	}
	return nil
}

// activityFingerprint is the holder-lifetime duplicate/conflict key: typed
// canonical envelope JSON, the marker flag and the registration context (nil for
// declaration-only events). Typed canonical JSON ignores insignificant outer
// field ordering/whitespace. Extension JSON retains member order and lexical
// numbers; this is not semantic JSON equality.
func activityFingerprint(v activityEnvelope, marker bool, registration *producerContext) string {
	data, _ := json.Marshal(struct {
		Envelope     activityEnvelope
		Marker       bool
		Registration *producerContext
	}{v, marker, registration})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func activityError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}
func increment(n *uint64) {
	if *n < ^uint64(0) {
		*n++
	}
}

func newActivityService(store *Store, state State, spec launchSpec, hook func(string) error) (*activityService, error) {
	return newActivityServiceClock(store, state, spec, hook, time.Now)
}
func newActivityServiceClock(store *Store, state State, spec launchSpec, hook func(string) error, now func() time.Time) (*activityService, error) {
	dir, e := store.dir(state.ID)
	if e != nil {
		return nil, e
	}
	// A legacy recording gets no activity endpoint; its holder refuses to run.
	if e = requireVisualFormat(state.Format); e != nil {
		return nil, e
	}
	ln, e := net.Listen("unix", filepath.Join(dir, "activity.sock"))
	if e != nil {
		return nil, e
	}
	if e = os.Chmod(filepath.Join(dir, "activity.sock"), 0600); e != nil {
		ln.Close()
		return nil, e
	}
	s := &activityService{store: store, state: State{ID: state.ID, Target: state.Target, Format: state.Format}, spec: spec, dir: filepath.Join(dir, activityIndexDir), listener: ln, hook: hook, closed: make(chan struct{}), finished: make(chan struct{})}
	s.now = now
	s.producers = map[string]*producerSlot{}
	s.connections = make(chan struct{}, activityConnectionsMax)
	s.sink = newIndexSink(s)
	go s.sink.run()
	go s.serve()
	return s, nil
}
func (s *activityService) close() {
	s.closeOnce.Do(func() { s.mu.Lock(); s.closing = true; close(s.closed); s.mu.Unlock(); s.listener.Close() })
}
func (s *activityService) point(point string) error {
	if s.hook != nil {
		return s.hook(point)
	}
	return nil
}
func (s *activityService) snapshot() activityResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return activityResponse{Accounting: s.accounting, Error: s.sinkErr}
}
func (s *activityService) reject(message string) activityResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	increment(&s.accounting.Rejected)
	return activityResponse{Accounting: s.accounting, Error: message}
}
func (s *activityService) reply(entry *activityEntry, duplicate bool) activityResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt := entry.receipt
	message := entry.err
	if message == "" && s.closing && !entry.durable {
		message = "holder closed before durability acknowledgement"
	}
	return activityResponse{Accepted: true, Durable: entry.durable, Failed: entry.failed, Duplicate: duplicate, Receipt: &receipt, Accounting: s.accounting, Error: message, RegisteredContext: entry.registration}
}
func (s *activityService) submit(v activityEnvelope, marker bool) activityResponse {
	return s.submitRegistered(v, marker, "")
}
func (s *activityService) submitRegistered(v activityEnvelope, marker bool, generation string) activityResponse {
	var registration *producerContext
	if generation != "" {
		descriptor := descriptorOf(v)
		if !validID.MatchString(generation) {
			return s.reject("invalid producer generation")
		}
		if e := validateProducer(descriptor, s.state.Target); e != nil {
			return s.reject(e.Error())
		}
		registration = &producerContext{Generation: generation, Descriptor: copyProducer(descriptor)}
	}
	return s.sink.admit(v, marker, registration)
}

func (s *activityService) serve() {
	slots := s.connections
	for {
		conn, e := s.listener.Accept()
		if e != nil {
			return
		}
		s.mu.Lock()
		increment(&s.accounting.Requests)
		s.mu.Unlock()
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots; conn.Close() }(); s.handle(conn) }()
		default:
			// Bounded refusal; cannot block the independent lifecycle listener.
			_ = conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
			_ = json.NewEncoder(conn).Encode(s.reject("activity connections saturated"))
			conn.Close()
		}
	}
}
func (s *activityService) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	data, e := io.ReadAll(io.LimitReader(conn, activityRequestMax+1))
	var req activityRequest
	var response activityResponse
	switch {
	case e != nil || len(data) > activityRequestMax:
		response = s.reject("invalid or oversized activity request")
	case strictActivityJSON(data, &req) != nil:
		response = s.reject("invalid activity request JSON")
	case req.Token != s.spec.Token || req.Recording != s.state.ID || req.Backend != s.state.Target.Backend || req.Instance != s.state.Target.Instance:
		response = s.reject("invalid activity binding/token")
	case activityEmptyRegistrationKeys(data, req):
		response = s.reject("empty registration fields")
	default:
		// Filesystem checks occur outside the machine and admission mutex. No backend
		// lookup, process lookup, launch, control call or implicit resume is involved.
		if e := s.checkBinding(); e != nil {
			response = s.reject("activity recording unavailable or superseded")
		} else if req.Operation == "producer-register" || req.Operation == "producer-refresh" || req.Operation == "producer-replace" {
			if req.Descriptor == nil || len(req.Envelope) != 0 {
				response = s.reject("invalid producer request")
			} else {
				response = s.producerOperation(req.Operation, *req.Descriptor, req.Generation)
			}
		} else if req.Operation == "status" {
			if len(req.Envelope) != 0 || req.Descriptor != nil || req.Generation != "" {
				response = s.reject("status cannot submit an envelope")
			} else {
				response = s.snapshot()
			}
		} else if req.Operation == "activity" || req.Operation == "marker" {
			v, e := parseActivity(req.Envelope, s.state.Target)
			if e != nil {
				response = s.reject(e.Error())
			} else if req.Descriptor != nil {
				response = s.reject("registered events require a generation and envelope identity only")
			} else {
				response = s.submitRegistered(v, req.Operation == "marker", req.Generation)
			}
		} else {
			response = s.reject("unknown activity operation")
		}
	}
	_ = json.NewEncoder(conn).Encode(response)
}

// An explicitly present but null/empty registration key is not omission: the
// request either carries a usable descriptor/generation or leaves the key out.
// This runs only after strict bounded request decoding, never on arbitrary input.
func activityEmptyRegistrationKeys(data []byte, req activityRequest) bool {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	_, descriptor := fields["descriptor"]
	_, generation := fields["generation"]
	return (descriptor && req.Descriptor == nil) || (generation && req.Generation == "")
}
func (s *activityService) checkBinding() error {
	var current string
	if _, e := offlineJSON(s.store.targetPath(s.state.Target), 128, &current); e != nil {
		return e
	}
	if current != s.state.ID {
		return fmt.Errorf("superseded recording")
	}
	dir, _ := s.store.dir(s.state.ID)
	var state State
	if _, e := offlineJSON(filepath.Join(dir, "state.json"), 4<<20, &state); e != nil {
		return e
	}
	if state.ID != s.state.ID || !state.Target.Equal(s.state.Target) || state.Status == "stopped" {
		return fmt.Errorf("inactive or mismatched recording")
	}
	return nil
}
